// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// probeTestAgent answers GetKubeadmStatus with err, or with a version when err is nil.
type probeTestAgent struct {
	nvkeproto.UnimplementedNodeAgentServer
	err error
}

func (a *probeTestAgent) GetKubeadmStatus(context.Context, *nvkeproto.KubeadmStatusRequest) (*nvkeproto.KubeadmStatusResponse, error) {
	if a.err != nil {
		return nil, a.err
	}
	return &nvkeproto.KubeadmStatusResponse{AgentVersion: "probe-test"}, nil
}

func (a *probeTestAgent) StreamExecuteScript(*nvkeproto.ExecuteScriptRequest, nvkeproto.NodeAgent_StreamExecuteScriptServer) error {
	return a.err
}

// serveProbeTestAgent starts a gRPC server on a loopback port and returns its address.
func serveProbeTestAgent(t *testing.T, service nvkeproto.NodeAgentServer, opts ...grpc.ServerOption) (string, string) {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(opts...)
	if service != nil {
		nvkeproto.RegisterNodeAgentServer(server, service)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return host, port
}

// skipIfAnswered skips a test that needs address to stay silent on a network
// that answers for it instead, such as one whose firewall rejects outbound
// connections.
func skipIfAnswered(t *testing.T, address string) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", address)
	if err == nil {
		_ = conn.Close()
		t.Skipf("%s accepted a connection on this network", address)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		t.Skipf("this network refuses connections to %s: %v", address, err)
	}
}

// closedPort returns a loopback port with nothing listening on it.
func closedPort(t *testing.T) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	return port
}

// resetAfterAccept accepts each connection and resets it at once.
func resetAfterAccept(t *testing.T) (string, string) {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return host, port
}

func probeTestCertificate(t *testing.T, commonName string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// mtlsAgent starts an agent that requires a client certificate signed by
// trusted, speaking at most maxVersion.
func mtlsAgent(t *testing.T, trusted tls.Certificate, maxVersion uint16) (string, string) {
	t.Helper()

	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(trusted.Leaf)
	serverTLS := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   maxVersion,
		Certificates: []tls.Certificate{probeTestCertificate(t, "agent", x509.ExtKeyUsageServerAuth)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	return serveProbeTestAgent(t, &probeTestAgent{}, grpc.Creds(credentials.NewTLS(serverTLS)))
}

func probe(t *testing.T, transport *Transport, node db.Node, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	_, err := (&NodeAgentClient{transport: transport}).GetAgentVersion(ctx, node)
	return err
}

func insecureTransport(port string) *Transport {
	return &Transport{cfg: config.Config{NodeAgent: config.NodeAgentConfig{Port: port}}}
}

// Each cause comes from a real connection through the production Transport.
func TestProbeCauseFromRealTransport(t *testing.T) {
	t.Parallel()

	trustedClient := probeTestCertificate(t, "controller", x509.ExtKeyUsageClientAuth)
	untrustedClient := probeTestCertificate(t, "other controller", x509.ExtKeyUsageClientAuth)

	tests := map[string]struct {
		setup   func(t *testing.T) (*Transport, db.Node)
		timeout time.Duration
		want    ProbeCause
	}{
		"nothing listens on the agent port": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				return insecureTransport(closedPort(t)), db.Node{IP: "127.0.0.1"}
			},
			want: ProbeCauseConnectionRefused,
		},
		"unroutable address": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				// TEST-NET-1 (RFC 5737) is never routed.
				skipIfAnswered(t, "192.0.2.1:15005")
				return insecureTransport("15005"), db.Node{IP: "192.0.2.1"}
			},
			timeout: 500 * time.Millisecond,
			want:    ProbeCauseUnreachable,
		},
		"connection reset after accept": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := resetAfterAccept(t)
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseConnectionLost,
		},
		"agent rejects the controller certificate during a TLS 1.2 handshake": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := mtlsAgent(t, trustedClient, tls.VersionTLS12)
				transport := insecureTransport(port)
				transport.externalClientCertificate = &untrustedClient
				return transport, db.Node{IP: host, NodeAgentExternalMTLSEnabled: true}
			},
			want: ProbeCauseAuthFailed,
		},
		"agent rejects the controller certificate after a TLS 1.3 handshake": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := mtlsAgent(t, trustedClient, tls.VersionTLS13)
				transport := insecureTransport(port)
				transport.externalClientCertificate = &untrustedClient
				return transport, db.Node{IP: host, NodeAgentExternalMTLSEnabled: true}
			},
			want: ProbeCauseAuthFailed,
		},
		"agent answers Unauthenticated": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(codes.Unauthenticated, "token rejected")})
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAuthFailed,
		},
		"agent answers PermissionDenied": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(codes.PermissionDenied, "subject not allowed")})
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAuthFailed,
		},
		"agent does not implement the method": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, nil)
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAgentError,
		},
		"agent answers Unavailable": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(codes.Unavailable, "draining")})
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAgentError,
		},
		"agent answers DeadlineExceeded": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(codes.DeadlineExceeded, "kubeadm timed out")})
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAgentError,
		},
		"agent answers Canceled": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(codes.Canceled, "shutting down")})
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAgentError,
		},
		"agent answers Internal": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(codes.Internal, "kubeadm status unreadable")})
				return insecureTransport(port), db.Node{IP: host}
			},
			want: ProbeCauseAgentError,
		},
		"external mTLS client certificate missing": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				return insecureTransport(closedPort(t)), db.Node{IP: "127.0.0.1", NodeAgentExternalMTLSEnabled: true}
			},
			want: ProbeCauseTransportSetup,
		},
		"SPIFFE without a trust domain": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				return insecureTransport(closedPort(t)), db.Node{IP: "127.0.0.1", SpiffeEnabled: true}
			},
			want: ProbeCauseTransportSetup,
		},
		"SPIFFE Workload API unavailable": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				transport := insecureTransport(closedPort(t))
				transport.trustDomain = "example.org"
				transport.cfg.SpiffeEndpointSocket = "unix://" + filepath.Join(t.TempDir(), "missing.sock")
				return transport, db.Node{IP: "127.0.0.1", SpiffeEnabled: true}
			},
			timeout: 500 * time.Millisecond,
			want:    ProbeCauseTransportSetup,
		},
		"TLS/JWT without a verifier": {
			setup: func(t *testing.T) (*Transport, db.Node) {
				t.Helper()
				transport := insecureTransport(closedPort(t))
				transport.externalClientCertificate = &trustedClient
				return transport, db.Node{IP: "127.0.0.1", NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true}
			},
			want: ProbeCauseTransportSetup,
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			timeout := params.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			transport, node := params.setup(t)
			err := probe(t, transport, node, timeout)
			require.Error(t, err)

			assert.Equal(t, params.want, CauseOf(err), "error: %v", err)
			var probeErr *ProbeError
			require.ErrorAs(t, err, &probeErr)
			assert.Equal(t, probeErr.Err.Error(), probeErr.Error(), "the cause must not change the error text")
		})
	}
}

// The status code still reaches callers that switch on it, such as the
// reboot path's Unimplemented fallback.
func TestProbeErrorKeepsTheStatusCode(t *testing.T) {
	t.Parallel()

	host, port := serveProbeTestAgent(t, nil)
	err := probe(t, insecureTransport(port), db.Node{IP: host}, 5*time.Second)

	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	assert.Contains(t, err.Error(), "failed to get kubeadm status from node agent")
}

// A client that rejects the agent's identity fails in its own handshake, the
// way SPIFFE's AuthorizeID does on a mismatched SPIFFE ID.
func TestProbeCauseWhenTheControllerRejectsTheAgentIdentity(t *testing.T) {
	t.Parallel()

	serverTLS := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{probeTestCertificate(t, "agent", x509.ExtKeyUsageServerAuth)},
	}
	host, port := serveProbeTestAgent(t, &probeTestAgent{}, grpc.Creds(credentials.NewTLS(serverTLS)))
	clientTLS := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // The test rejects the peer in VerifyPeerCertificate instead.
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
			return errors.New("unexpected ID \"spiffe://example.org/other-node\"")
		},
	}

	recorder := &connRecorder{}
	conn, err := grpc.NewClient(net.JoinHostPort(host, port), recorder.dialOptions(credentials.NewTLS(clientTLS))...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = nvkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(ctx, &nvkeproto.KubeadmStatusRequest{})

	require.Error(t, err)
	assert.Equal(t, ProbeCauseAuthFailed, CauseOf(err), "error: %v", err)
}

// The agent's trailers can arrive just before the probe deadline closes the
// call with gRPC's own status. That status did not come from the agent.
func TestProbeCauseWhenTheDeadlineRacesTheAgentAnswer(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		endCall bool
		want    ProbeCause
	}{
		"the agent's status reaches the caller":                {endCall: false, want: ProbeCauseAgentError},
		"the deadline ends the call after the trailers arrive": {endCall: true, want: ProbeCauseUnknown},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			recorder := &connRecorder{}
			recorder.record(connEventSecured, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
				answerWatcher{}.HandleRPC(ctx, &stats.InTrailer{})
				if params.endCall {
					cancel()
				}
				return status.Error(codes.Canceled, "the call ended")
			}

			err := recorder.unaryInterceptor(ctx, "/nvke.agent.v1.NodeAgent/GetAgentVersion", nil, nil, nil, invoker)

			assert.Equal(t, params.want, CauseOf(err), "error: %v", err)
		})
	}
}

// A status the agent returns on a stream is the agent's answer, even though
// gRPC has already canceled the stream's own context when RecvMsg returns it.
func TestProbeCauseOfAStatusTheAgentReturnsOnAStream(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()

			host, port := serveProbeTestAgent(t, &probeTestAgent{err: status.Error(code, "from the agent")})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, closeConn, err := insecureTransport(port).Dial(ctx, db.Node{IP: host})
			require.NoError(t, err)
			defer closeConn()

			stream, err := nvkeproto.NewNodeAgentClient(conn).StreamExecuteScript(ctx, &nvkeproto.ExecuteScriptRequest{})
			require.NoError(t, err)
			_, err = stream.Recv()

			require.Error(t, err)
			assert.Equal(t, code, status.Code(err))
			assert.Equal(t, ProbeCauseAgentError, CauseOf(err), "error: %v", err)
		})
	}
}

func TestCauseOfAnErrorWithoutARecordedCause(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want ProbeCause
	}{
		"nil":                {nil, ""},
		"deadline":           {fmt.Errorf("probing: %w", context.DeadlineExceeded), ProbeCauseUnreachable},
		"canceled":           {context.Canceled, ProbeCauseUnreachable},
		"refused":            {errors.New("dial tcp 10.0.0.1:15005: connect: connection refused"), ProbeCauseConnectionRefused},
		"io timeout":         {errors.New("dial tcp 10.0.0.1:15005: i/o timeout"), ProbeCauseUnreachable},
		"no route":           {errors.New("dial tcp 10.0.0.1:15005: connect: no route to host"), ProbeCauseUnreachable},
		"network down":       {errors.New("dial tcp 10.0.0.1:15005: connect: network is unreachable"), ProbeCauseUnreachable},
		"reset":              {errors.New("read tcp 10.0.0.2:41234->10.0.0.1:15005: read: connection reset by peer"), ProbeCauseConnectionLost},
		"broken pipe":        {errors.New("write tcp 10.0.0.2:41234->10.0.0.1:15005: write: broken pipe"), ProbeCauseConnectionLost},
		"closed":             {errors.New("error reading server preface: EOF"), ProbeCauseConnectionLost},
		"handshake":          {errors.New("authentication handshake failed: remote error: tls: bad certificate"), ProbeCauseAuthFailed},
		"x509":               {errors.New("x509: certificate has expired or is not yet valid"), ProbeCauseAuthFailed},
		"permission denied":  {errors.New("rpc error: code = PermissionDenied desc = subject not allowed"), ProbeCauseAuthFailed},
		"unrecognized error": {errors.New("something else"), ProbeCauseUnknown},
		"recorded cause wins": {
			fmt.Errorf("wrapped: %w", &ProbeError{Cause: ProbeCauseAgentError, Err: errors.New("connection refused")}),
			ProbeCauseAgentError,
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, params.want, CauseOf(params.err))
		})
	}
}

func TestTransportProfile(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		node db.Node
		want string
	}{
		"insecure":              {db.Node{}, "insecure"},
		"spiffe":                {db.Node{SpiffeEnabled: true}, "spiffe"},
		"external mTLS":         {db.Node{NodeAgentExternalMTLSEnabled: true, SpiffeEnabled: true}, "externalMTLS"},
		"external mTLS and JWT": {db.Node{NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true}, "externalMTLS+tlsJWT"},
		"spiffe and JWT":        {db.Node{SpiffeEnabled: true, NodeAgentTLSJWTEnabled: true}, "spiffe+tlsJWT"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, params.want, TransportProfile(params.node))
		})
	}
}
