// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/dsx-ai-factory/nvke/agent/pkg/nkeauth"
)

// authRecordTimeout bounds the base handshake and the record read together.
const authRecordTimeout = 15 * time.Second

// authRecordCredentials reads the NKEAuth record an agent in TLS/JWT mode
// writes after the TLS handshake. The checker is a diagnostic, not the
// controller: it consumes the record so gRPC sees a clean HTTP/2 stream, and
// reports the token's claims without verifying its signature.
type authRecordCredentials struct {
	base credentials.TransportCredentials
	// state is shared with every clone, because gRPC may clone the
	// credentials before handshaking and the probe reads the result from the
	// value it constructed.
	state *authRecordState
}

type authRecordState struct {
	mu      sync.Mutex
	subject string
	seen    bool
}

func newAuthRecordCredentials(base credentials.TransportCredentials) *authRecordCredentials {
	return &authRecordCredentials{base: base, state: &authRecordState{}}
}

func (c *authRecordCredentials) ClientHandshake(
	ctx context.Context,
	authority string,
	rawConn net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	deadline := time.Now().Add(authRecordTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := rawConn.SetDeadline(deadline); err != nil {
		return nil, nil, fmt.Errorf("setting authentication record deadline: %w", err)
	}

	conn, authInfo, err := c.base.ClientHandshake(ctx, authority, rawConn)
	if err != nil {
		return nil, nil, err
	}

	token, err := nkeauth.ReadJWT(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("reading nke-agent authentication record: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("clearing authentication record deadline: %w", err)
	}

	c.state.mu.Lock()
	c.state.seen = true
	c.state.subject = unverifiedSubject(token)
	c.state.mu.Unlock()

	return conn, authInfo, nil
}

// observed reports whether a record arrived and the subject it claimed.
func (c *authRecordCredentials) observed() (bool, string) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	return c.state.seen, c.state.subject
}

func (c *authRecordCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("authentication record credentials support client handshakes only")
}

func (c *authRecordCredentials) Info() credentials.ProtocolInfo {
	info := c.base.Info()
	info.SecurityProtocol = "tls-jwt"
	return info
}

func (c *authRecordCredentials) Clone() credentials.TransportCredentials {
	return &authRecordCredentials{base: c.base.Clone(), state: c.state}
}

func (c *authRecordCredentials) OverrideServerName(serverNameOverride string) error {
	//nolint:staticcheck // Required by the gRPC TransportCredentials interface.
	return c.base.OverrideServerName(serverNameOverride)
}

// unverifiedSubject reads the sub claim without checking the signature. The
// controller is the only component that validates this token; the checker
// shows the claim so a node-identity mismatch is visible on the node itself.
func unverifiedSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Subject
}
