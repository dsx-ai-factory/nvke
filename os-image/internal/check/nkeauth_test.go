// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/dsx-ai-factory/nke/agent/pkg/nkeauth"
)

// stubBaseCredentials stands in for the SPIFFE mTLS credentials the checker
// wraps, so the tests exercise only the record read.
type stubBaseCredentials struct {
	err error
}

func (s *stubBaseCredentials) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	return conn, nil, nil
}

func (s *stubBaseCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, nil, nil
}

func (s *stubBaseCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}

func (s *stubBaseCredentials) Clone() credentials.TransportCredentials {
	return &stubBaseCredentials{err: s.err}
}

func (s *stubBaseCredentials) OverrideServerName(string) error { return nil }

// testToken builds an unsigned JWT-shaped string carrying sub.
func testToken(subject string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + subject + `"}`))
	return header + "." + payload + ".signature"
}

func TestClientHandshakeReadsRecordAndLeavesStream(t *testing.T) {
	t.Parallel()
	clientConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = agentConn.Close() })

	const subject = "spiffe://dsx.example/bm/2f1c0b8e-7a3d-4c21-9f55-6b0d8e2a1c44"
	preface := []byte("PRI * HTTP/2.0\r\n")
	go func() {
		if err := nkeauth.WriteJWT(agentConn, testToken(subject)); err != nil {
			return
		}
		_, _ = agentConn.Write(preface)
	}()

	creds := newAuthRecordCredentials(&stubBaseCredentials{})
	conn, _, err := creds.ClientHandshake(context.Background(), "agent", clientConn)
	if err != nil {
		t.Fatalf("ClientHandshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	seen, gotSubject := creds.observed()
	if !seen {
		t.Fatal("expected the credentials to record that a record arrived")
	}
	if gotSubject != subject {
		t.Fatalf("subject = %q, want %q", gotSubject, subject)
	}

	// gRPC must find the HTTP/2 preface as the first unread byte.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	rest := make([]byte, len(preface))
	if _, err := io.ReadFull(conn, rest); err != nil {
		t.Fatalf("reading after the record: %v", err)
	}
	if string(rest) != string(preface) {
		t.Fatalf("stream after the record = %q, want %q", rest, preface)
	}
}

func TestClientHandshakeFailsClosed(t *testing.T) {
	t.Parallel()
	baseErr := errors.New("certificate rejected")
	cases := []struct {
		name    string
		base    *stubBaseCredentials
		agent   func(net.Conn)
		wantErr string
	}{
		{
			name:    "base handshake failure",
			base:    &stubBaseCredentials{err: baseErr},
			agent:   func(net.Conn) {},
			wantErr: baseErr.Error(),
		},
		{
			name:    "agent sends http/2 without a record",
			base:    &stubBaseCredentials{},
			agent:   func(conn net.Conn) { _, _ = conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")) },
			wantErr: "invalid protocol magic",
		},
		{
			name:    "agent closes before the record",
			base:    &stubBaseCredentials{},
			agent:   func(conn net.Conn) { _ = conn.Close() },
			wantErr: "reading header",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clientConn, agentConn := net.Pipe()
			t.Cleanup(func() { _ = agentConn.Close() })
			go tc.agent(agentConn)

			creds := newAuthRecordCredentials(tc.base)
			_, _, err := creds.ClientHandshake(context.Background(), "agent", clientConn)
			if err == nil {
				t.Fatal("expected ClientHandshake to fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if seen, _ := creds.observed(); seen {
				t.Fatal("a failed handshake must not record a token")
			}
		})
	}
}

func TestUnverifiedSubject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{name: "well formed", token: testToken("spiffe://dsx.example/bm/node-1"), want: "spiffe://dsx.example/bm/node-1"},
		{name: "not a jwt", token: "opaque-token", want: ""},
		{name: "payload is not base64", token: "a.!!!.c", want: ""},
		{
			name:  "payload is not json",
			token: "a." + base64.RawURLEncoding.EncodeToString([]byte("plain")) + ".c",
			want:  "",
		},
		{
			name:  "no sub claim",
			token: "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"aud":"nke-controller"}`)) + ".c",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := unverifiedSubject(tc.token); got != tc.want {
				t.Fatalf("unverifiedSubject() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloneSharesObservedState(t *testing.T) {
	t.Parallel()
	clientConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = agentConn.Close() })

	const subject = "spiffe://dsx.example/bm/node-1"
	go func() { _ = nkeauth.WriteJWT(agentConn, testToken(subject)) }()

	creds := newAuthRecordCredentials(&stubBaseCredentials{})
	conn, _, err := creds.Clone().ClientHandshake(context.Background(), "agent", clientConn)
	if err != nil {
		t.Fatalf("ClientHandshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	seen, gotSubject := creds.observed()
	if !seen || gotSubject != subject {
		t.Fatalf("observed() = (%t, %q) on the original after a clone handshook, want (true, %q)", seen, gotSubject, subject)
	}
}

func TestServerHandshakeRefused(t *testing.T) {
	t.Parallel()
	creds := newAuthRecordCredentials(&stubBaseCredentials{})
	if _, _, err := creds.ServerHandshake(nil); err == nil {
		t.Fatal("expected the checker credentials to refuse a server handshake")
	}
}

func TestInfoReportsTLSJWT(t *testing.T) {
	t.Parallel()
	creds := newAuthRecordCredentials(&stubBaseCredentials{})
	if got := creds.Info().SecurityProtocol; got != "tls-jwt" {
		t.Fatalf("SecurityProtocol = %q, want %q", got, "tls-jwt")
	}
}
