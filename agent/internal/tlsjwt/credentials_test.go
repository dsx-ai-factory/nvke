// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials"

	"github.com/dsx-ai-factory/nke/agent/pkg/nkeauth"
)

// stubBaseCredentials stands in for the agent's SPIFFE mTLS server credentials.
// Returning the connection unchanged keeps these tests on the record exchange
// rather than on TLS, which go-spiffe already covers.
type stubBaseCredentials struct {
	err        error
	handshakes atomic.Int32
}

func (c *stubBaseCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	c.handshakes.Add(1)
	if c.err != nil {
		return nil, nil, c.err
	}
	return conn, nil, nil
}

func (c *stubBaseCredentials) ClientHandshake(
	context.Context,
	string,
	net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("not implemented")
}

func (c *stubBaseCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.3"}
}

func (c *stubBaseCredentials) Clone() credentials.TransportCredentials {
	return &stubBaseCredentials{err: c.err}
}

func (c *stubBaseCredentials) OverrideServerName(string) error { return nil }

type stubTokenSource struct {
	token string
	err   error
	calls atomic.Int32
}

func (s *stubTokenSource) Token(context.Context) (string, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

func TestNewServerCredentials_RequiresItsCollaborators(t *testing.T) {
	t.Parallel()

	t.Run("nil base", func(t *testing.T) {
		t.Parallel()

		creds, err := NewServerCredentials(nil, &stubTokenSource{token: "dsx"}, nil)
		assert.Nil(t, creds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "base transport credentials")
	})

	t.Run("nil token source", func(t *testing.T) {
		t.Parallel()

		creds, err := NewServerCredentials(&stubBaseCredentials{}, nil, nil)
		assert.Nil(t, creds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token source")
	})
}

func TestServerHandshake_WritesOneRecordThenLeavesTheStreamToGRPC(t *testing.T) {
	t.Parallel()

	const token = "dsx-identity-token"
	const followingBytes = "PRI * HTTP/2.0\r\n"

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	tokens := &stubTokenSource{token: token}
	creds, err := NewServerCredentials(&stubBaseCredentials{}, tokens, nil)
	require.NoError(t, err)

	handshake := make(chan error, 1)
	go func() {
		conn, _, handshakeErr := creds.ServerHandshake(serverConn)
		if handshakeErr != nil {
			handshake <- handshakeErr
			return
		}
		// gRPC owns the stream from here; stand in for its first write.
		_, writeErr := io.WriteString(conn, followingBytes)
		handshake <- writeErr
	}()

	got, err := nkeauth.ReadJWT(clientConn)
	require.NoError(t, err)
	assert.Equal(t, token, got)

	// ReadJWT must consume exactly the record and leave HTTP/2 bytes intact.
	following := make([]byte, len(followingBytes))
	_, err = io.ReadFull(clientConn, following)
	require.NoError(t, err)
	assert.Equal(t, followingBytes, string(following))

	require.NoError(t, <-handshake)
	assert.Equal(t, int32(1), tokens.calls.Load())
}

func TestServerHandshake_SendsNoTokenWhenTheBaseHandshakeFails(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	tokens := &stubTokenSource{token: "dsx-identity-token"}
	base := &stubBaseCredentials{err: errors.New("remote error: tls: bad certificate")}
	creds, err := NewServerCredentials(base, tokens, nil)
	require.NoError(t, err)

	conn, authInfo, err := creds.ServerHandshake(serverConn)
	assert.Nil(t, conn)
	assert.Nil(t, authInfo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad certificate")

	// An unauthenticated controller must not cause a token to be minted, let
	// alone written.
	assert.Equal(t, int32(0), tokens.calls.Load())
}

func TestServerHandshake_ClosesTheConnectionWhenNoTokenIsAvailable(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	tokens := &stubTokenSource{err: errors.New("connection refused")}
	creds, err := NewServerCredentials(&stubBaseCredentials{}, tokens, nil)
	require.NoError(t, err)

	conn, _, err := creds.ServerHandshake(serverConn)
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "obtaining node-agent identity token")

	// The peer sees a closed connection rather than a stream it could mistake
	// for an authenticated one.
	_, readErr := clientConn.Read(make([]byte, 1))
	require.Error(t, readErr)
}

func TestServerHandshake_FailsWhenTheTokenExceedsTheRecordLimit(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	tokens := &stubTokenSource{token: strings.Repeat("a", nkeauth.MaxJWTSize+1)}
	creds, err := NewServerCredentials(&stubBaseCredentials{}, tokens, nil)
	require.NoError(t, err)

	conn, _, err := creds.ServerHandshake(serverConn)
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writing node-agent authentication record")
}

func TestServerHandshake_BoundsTheExchangeWithADeadline(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	tokens := &stubTokenSource{token: "dsx-identity-token"}
	creds, err := NewServerCredentials(&stubBaseCredentials{}, tokens, nil)
	require.NoError(t, err)
	creds.timeout = 50 * time.Millisecond

	// Nothing reads the pipe, so the record write blocks until the deadline
	// rather than hanging the accept loop.
	conn, _, err := creds.ServerHandshake(serverConn)
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writing node-agent authentication record")
}

func TestClientHandshake_IsRefused(t *testing.T) {
	t.Parallel()

	creds, err := NewServerCredentials(&stubBaseCredentials{}, &stubTokenSource{token: "dsx"}, nil)
	require.NoError(t, err)

	conn, authInfo, err := creds.ClientHandshake(context.Background(), "authority", nil)
	assert.Nil(t, conn)
	assert.Nil(t, authInfo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server handshakes only")
}

func TestInfoAndClone_PreserveTheBaseTransport(t *testing.T) {
	t.Parallel()

	creds, err := NewServerCredentials(&stubBaseCredentials{}, &stubTokenSource{token: "dsx"}, nil)
	require.NoError(t, err)

	info := creds.Info()
	assert.Equal(t, "tls-jwt", info.SecurityProtocol)
	assert.Equal(t, "1.3", info.SecurityVersion)

	clone, ok := creds.Clone().(*ServerCredentials)
	require.True(t, ok)
	assert.Equal(t, "tls-jwt", clone.Info().SecurityProtocol)
	assert.NotSame(t, creds, clone)
	assert.NoError(t, clone.OverrideServerName("ignored"))
}
