// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ProbeCause is why a node-agent call failed.
type ProbeCause string

// The closed set of node-agent failure causes.
const (
	// ProbeCauseNoAddress means the node has no IP address to dial.
	ProbeCauseNoAddress ProbeCause = "NoAddress"
	// ProbeCauseUnreachable means no TCP connection was made within the deadline.
	ProbeCauseUnreachable ProbeCause = "Unreachable"
	// ProbeCauseConnectionRefused means the host answered and nothing listens on the agent port.
	ProbeCauseConnectionRefused ProbeCause = "ConnectionRefused"
	// ProbeCauseConnectionLost means the connection was made, then reset or closed before the call completed.
	ProbeCauseConnectionLost ProbeCause = "ConnectionLost"
	// ProbeCauseAuthFailed means the two sides did not accept each other's identity.
	ProbeCauseAuthFailed ProbeCause = "AuthFailed"
	// ProbeCauseAgentError means the agent answered with a gRPC error status.
	ProbeCauseAgentError ProbeCause = "AgentError"
	// ProbeCauseTransportSetup means the controller could not build the node's credentials.
	ProbeCauseTransportSetup ProbeCause = "TransportSetup"
	// ProbeCauseUnknown means the failure fits none of the other causes.
	ProbeCauseUnknown ProbeCause = "Unknown"
)

// ProbeError is a failed node-agent call and its cause.
type ProbeError struct {
	Cause ProbeCause
	Err   error
}

func (e *ProbeError) Error() string { return e.Err.Error() }

func (e *ProbeError) Unwrap() error { return e.Err }

// CauseOf returns the cause of a failed node-agent call, or "" for a nil error.
// Calls made through a Transport carry their cause; any other error is
// classified by its text, which can change between gRPC releases.
func CauseOf(err error) ProbeCause {
	if err == nil {
		return ""
	}
	var probeErr *ProbeError
	if errors.As(err, &probeErr) {
		return probeErr.Cause
	}
	return causeFromText(err)
}

// TransportProfile names the transport a node is dialed with.
func TransportProfile(node db.Node) string {
	profile := "insecure"
	switch {
	case node.NodeAgentExternalMTLSEnabled:
		profile = "externalMTLS"
	case node.SpiffeEnabled:
		profile = "spiffe"
	}
	if node.NodeAgentTLSJWTEnabled {
		profile += "+tlsJWT"
	}
	return profile
}

func transportSetupError(err error) error {
	return &ProbeError{Cause: ProbeCauseTransportSetup, Err: err}
}

// connEventKind is the last thing one dial's connection attempts reported.
type connEventKind int

const (
	connEventNone connEventKind = iota
	connEventDialFailed
	connEventHandshakeFailed
	connEventSecured
)

// connRecorder records what happened on the connections of one Dial, so a
// failed call can be classified from the underlying net and TLS errors, which
// gRPC flattens into a status message.
type connRecorder struct {
	mu    sync.Mutex
	event connEventKind
	err   error
}

func (r *connRecorder) record(event connEventKind, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.event = event
	r.err = err
}

func (r *connRecorder) last() (connEventKind, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.event, r.err
}

// dial is the connection dialer for one Dial. It replaces gRPC's dialer to
// record the dial error, and keeps its TCP keepalive at the OS defaults. A
// custom dialer bypasses gRPC's proxy support; agent connections are direct.
func (r *connRecorder) dial(ctx context.Context, address string) (net.Conn, error) {
	dialer := net.Dialer{KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: -1, Interval: -1, Count: -1}}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		r.record(connEventDialFailed, err)
	}
	return conn, err
}

// dialOptions installs the recorder on a connection that uses creds.
func (r *connRecorder) dialOptions(creds credentials.TransportCredentials) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(&recordingCredentials{TransportCredentials: creds, recorder: r}),
		grpc.WithContextDialer(r.dial),
		grpc.WithChainUnaryInterceptor(r.unaryInterceptor),
		grpc.WithChainStreamInterceptor(r.streamInterceptor),
	}
}

func (r *connRecorder) unaryInterceptor(
	ctx context.Context,
	method string,
	req, reply any,
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	return r.classify(invoker(ctx, method, req, reply, cc, opts...))
}

func (r *connRecorder) streamInterceptor(
	ctx context.Context,
	desc *grpc.StreamDesc,
	cc *grpc.ClientConn,
	method string,
	streamer grpc.Streamer,
	opts ...grpc.CallOption,
) (grpc.ClientStream, error) {
	stream, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		return nil, r.classify(err)
	}
	return &recordingStream{ClientStream: stream, recorder: r}, nil
}

// classify wraps a failed call's error in a ProbeError with its cause.
func (r *connRecorder) classify(err error) error {
	if err == nil {
		return nil
	}
	var probeErr *ProbeError
	if errors.As(err, &probeErr) {
		return err
	}
	return &ProbeError{Cause: r.cause(err), Err: err}
}

func (r *connRecorder) cause(err error) ProbeCause {
	code := status.Code(err)
	switch code { //nolint:exhaustive // the default covers every code the agent returns itself
	case codes.Unauthenticated, codes.PermissionDenied:
		return ProbeCauseAuthFailed
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		// gRPC itself reports a failed connection with these codes, so the
		// recorded connection outcome says which it was.
	default:
		return ProbeCauseAgentError
	}

	event, eventErr := r.last()
	switch event {
	case connEventDialFailed:
		return dialCause(eventErr)
	case connEventHandshakeFailed:
		return handshakeCause(eventErr)
	case connEventSecured:
		if code == codes.Unavailable {
			return ProbeCauseConnectionLost
		}
		// The connection stood, and the call ran out of time or was
		// canceled before the agent answered.
		return ProbeCauseUnknown
	case connEventNone:
		if code != codes.Unavailable {
			// The deadline expired before any connection attempt finished.
			return ProbeCauseUnreachable
		}
	}
	return causeFromText(err)
}

func dialCause(err error) ProbeCause {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return ProbeCauseConnectionRefused
	case isConnectionLost(err):
		return ProbeCauseConnectionLost
	default:
		// A timeout, no route, an unreachable network, a failed name lookup or
		// an expired deadline: no TCP connection was made. Timeout and no route
		// are one class, because the probe deadline and gRPC's connect timeout
		// decide which of the two appears.
		return ProbeCauseUnreachable
	}
}

func handshakeCause(err error) ProbeCause {
	if isConnectionLost(err) {
		return ProbeCauseConnectionLost
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ProbeCauseUnreachable
	}
	return ProbeCauseAuthFailed
}

func isConnectionLost(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

// causeFromText classifies an error that carries no recorded cause. Each
// phrase is pinned by a test, because gRPC can reword its messages.
func causeFromText(err error) ProbeCause {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ProbeCauseUnreachable
	}
	text := strings.ToLower(err.Error())
	contains := func(phrases ...string) bool {
		for _, phrase := range phrases {
			if strings.Contains(text, phrase) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("connection refused"):
		return ProbeCauseConnectionRefused
	case contains("i/o timeout", "no route to host", "network is unreachable", "deadline exceeded"):
		return ProbeCauseUnreachable
	case contains("connection reset", "broken pipe", "connection closed", "eof"):
		return ProbeCauseConnectionLost
	case contains("handshake", "x509", "certificate", "unauthenticated", "permissiondenied", "permission denied"):
		return ProbeCauseAuthFailed
	default:
		return ProbeCauseUnknown
	}
}

// recordingCredentials records the outcome of each client handshake.
type recordingCredentials struct {
	credentials.TransportCredentials
	recorder *connRecorder
}

func (c *recordingCredentials) ClientHandshake(
	ctx context.Context,
	authority string,
	rawConn net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	conn, authInfo, err := c.TransportCredentials.ClientHandshake(ctx, authority, rawConn)
	if err != nil {
		c.recorder.record(connEventHandshakeFailed, err)
		return conn, authInfo, err
	}
	c.recorder.record(connEventSecured, nil)
	return &recordingConn{Conn: conn, recorder: c.recorder}, authInfo, nil
}

func (c *recordingCredentials) Clone() credentials.TransportCredentials {
	return &recordingCredentials{TransportCredentials: c.TransportCredentials.Clone(), recorder: c.recorder}
}

// recordingConn records a TLS alert received after the handshake. In TLS 1.3
// the client finishes its handshake before the server checks the client
// certificate, so a rejected controller identity arrives as an alert on the
// first read.
type recordingConn struct {
	net.Conn
	recorder *connRecorder
}

func (c *recordingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	var opErr *net.OpError
	if err != nil && errors.As(err, &opErr) && opErr.Op == "remote error" {
		c.recorder.record(connEventHandshakeFailed, err)
	}
	return n, err
}

// recordingStream classifies a stream's receive errors. io.EOF ends a stream
// normally and passes through unchanged.
type recordingStream struct {
	grpc.ClientStream
	recorder *connRecorder
}

func (s *recordingStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return s.recorder.classify(err)
}
