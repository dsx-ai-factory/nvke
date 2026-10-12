// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/dsx-ai-factory/nvke/agent/pkg/nkeauth"
)

// HandshakeTimeout bounds the whole authentication exchange: the base TLS
// handshake, any token refresh it triggers, and the record write. It matches
// the controller's deadline for the same exchange.
const HandshakeTimeout = 15 * time.Second

var _ credentials.TransportCredentials = (*ServerCredentials)(nil)

// ServerCredentials wraps the agent's base TLS server credentials with the
// pre-gRPC authentication exchange. It completes the base handshake, which
// authenticates the controller's client certificate, and only then releases a
// DSX identity token onto the encrypted connection.
type ServerCredentials struct {
	base    credentials.TransportCredentials
	tokens  TokenSource
	timeout time.Duration
	logger  *slog.Logger
}

// NewServerCredentials wraps base so every accepted connection carries one
// NKEAuth record.
func NewServerCredentials(
	base credentials.TransportCredentials,
	tokens TokenSource,
	logger *slog.Logger,
) (*ServerCredentials, error) {
	if base == nil {
		return nil, errors.New("tls/jwt authentication requires base transport credentials")
	}
	if tokens == nil {
		return nil, errors.New("tls/jwt authentication requires a token source")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ServerCredentials{
		base:    base,
		tokens:  tokens,
		timeout: HandshakeTimeout,
		logger:  logger,
	}, nil
}

// ServerHandshake completes the base TLS handshake, writes one NKEAuth record,
// and returns the connection to gRPC. It sends no acknowledgement and waits for
// none: the controller signals acceptance by proceeding to HTTP/2. Any failure
// closes the connection, so there is no path from a failed exchange to serving
// an unauthenticated peer.
func (c *ServerCredentials) ServerHandshake(rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	deadline := time.Now().Add(c.timeout)
	if err := rawConn.SetDeadline(deadline); err != nil {
		return nil, nil, fmt.Errorf("setting tls/jwt handshake deadline: %w", err)
	}

	// A failed base handshake means the peer did not present an acceptable
	// controller certificate. No token is fetched and none is sent.
	conn, authInfo, err := c.base.ServerHandshake(rawConn)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (net.Conn, credentials.AuthInfo, error) {
		_ = conn.Close()
		return nil, nil, err
	}

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	token, err := c.tokens.Token(ctx)
	if err != nil {
		c.logger.Error("node-agent identity token unavailable", "remote_address", remoteAddress(conn), "err", err)
		return fail(fmt.Errorf("obtaining node-agent identity token: %w", err))
	}
	if err := nkeauth.WriteJWT(conn, token); err != nil {
		return fail(fmt.Errorf("writing node-agent authentication record: %w", err))
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(fmt.Errorf("clearing tls/jwt handshake deadline: %w", err))
	}

	c.logger.Debug("node-agent authentication record sent", "remote_address", remoteAddress(conn))
	return conn, authInfo, nil
}

// ClientHandshake reports that these credentials are server-only. The agent
// never dials a controller.
func (c *ServerCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("tls/jwt node-agent credentials support server handshakes only")
}

func (c *ServerCredentials) Info() credentials.ProtocolInfo {
	info := c.base.Info()
	info.SecurityProtocol = "tls-jwt"
	return info
}

func (c *ServerCredentials) Clone() credentials.TransportCredentials {
	clone := *c
	clone.base = c.base.Clone()
	return &clone
}

func (c *ServerCredentials) OverrideServerName(serverNameOverride string) error {
	// Required by credentials.TransportCredentials even though gRPC no longer
	// invokes it; delegate so callers using the interface still get its behavior.
	//nolint:staticcheck // Compatibility implementation for the gRPC interface.
	return c.base.OverrideServerName(serverNameOverride)
}

func remoteAddress(conn net.Conn) string {
	if addr := conn.RemoteAddr(); addr != nil {
		return addr.String()
	}
	return "unknown"
}
