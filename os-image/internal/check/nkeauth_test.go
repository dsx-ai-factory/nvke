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
		name  string
		base  *stubBaseCredentials
		agent func(net.Conn)
		// wantErrAny lists the error substrings that are acceptable for the
		// case. More than one entry means the handshake has more than one
		// legitimate place to notice the same failure, not that the assertion
		// is soft: every listed message still proves it failed closed.
		wantErrAny []string
		// closeAgentFirst closes the agent end and waits for it before the
		// handshake starts, pinning the ordering the racy case can only reach
		// by luck.
		closeAgentFirst bool
	}{
		{
			name:       "base handshake failure",
			base:       &stubBaseCredentials{err: baseErr},
			agent:      func(net.Conn) {},
			wantErrAny: []string{baseErr.Error()},
		},
		{
			name:       "agent sends http/2 without a record",
			base:       &stubBaseCredentials{},
			agent:      func(conn net.Conn) { _, _ = conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")) },
			wantErrAny: []string{"invalid protocol magic"},
		},
		{
			// The agent goroutine closes its end concurrently with
			// ClientHandshake, so which call notices is a genuine race and
			// depends on machine load -- this failed deterministically on a
			// loaded runner and passed 30/30 on a warm one.
			//
			// net.Pipe.SetDeadline returns io.ErrClosedPipe when *either* end
			// is done, so if the close lands first the handshake aborts at the
			// deadline call; otherwise it reaches the record read and fails
			// there. Both orderings are correct fail-closed behaviour, and the
			// observed() assertion below is what actually pins the security
			// property: no token is recorded either way.
			name:  "agent closes before the record",
			base:  &stubBaseCredentials{},
			agent: func(conn net.Conn) { _ = conn.Close() },
			wantErrAny: []string{
				"reading header",
				"setting authentication record deadline",
			},
		},
		{
			// The deterministic half of the case above: with the close already
			// complete, SetDeadline always loses. Without this, the
			// close-wins-the-race path is only ever covered by accident, which
			// is how it reached CI as a red build instead of as a test case.
			name:            "agent already closed when the handshake starts",
			base:            &stubBaseCredentials{},
			agent:           func(net.Conn) {},
			closeAgentFirst: true,
			wantErrAny:      []string{"setting authentication record deadline"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clientConn, agentConn := net.Pipe()
			t.Cleanup(func() { _ = agentConn.Close() })
			if tc.closeAgentFirst {
				_ = agentConn.Close()
			} else {
				go tc.agent(agentConn)
			}

			creds := newAuthRecordCredentials(tc.base)
			_, _, err := creds.ClientHandshake(context.Background(), "agent", clientConn)
			if err == nil {
				t.Fatal("expected ClientHandshake to fail")
			}
			if !containsAny(err.Error(), tc.wantErrAny) {
				t.Fatalf("error = %v, want it to contain one of %q", err, tc.wantErrAny)
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

// containsAny reports whether got contains at least one of the substrings.
func containsAny(got string, want []string) bool {
	for _, w := range want {
		if strings.Contains(got, w) {
			return true
		}
	}
	return false
}
