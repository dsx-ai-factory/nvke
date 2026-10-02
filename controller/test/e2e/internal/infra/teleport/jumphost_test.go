// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	testJumpUser     = "svc-nke-e2e"
	testJumpPassword = "not-the-real-one"
)

// fakeJumpHost is an sshd that authenticates one account by password and
// forwards direct-tcpip channels to a single address, whatever was asked for.
// What was asked for is recorded instead, so a test can assert the relay
// aimed at the machine's port 22.
type fakeJumpHost struct {
	t       *testing.T
	hostKey ssh.Signer

	offerPassword bool
	offerKeyboard bool
	allowForward  bool

	forwardTo string

	mu        sync.Mutex
	requested []string
}

func (j *fakeJumpHost) serve(conn net.Conn) {
	cfg := &ssh.ServerConfig{}
	if j.offerPassword {
		cfg.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() != testJumpUser || string(password) != testJumpPassword {
				return nil, errors.New("denied")
			}
			return &ssh.Permissions{}, nil
		}
	}
	if j.offerKeyboard {
		cfg.KeyboardInteractiveCallback = func(meta ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := challenge("", "", []string{"Password: "}, []bool{false})
			if err != nil {
				return nil, err
			}
			if meta.User() != testJumpUser || len(answers) != 1 || answers[0] != testJumpPassword {
				return nil, errors.New("denied")
			}
			return &ssh.Permissions{}, nil
		}
	}
	cfg.AddHostKey(j.hostKey)

	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sconn.Close() }()
	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		var target struct {
			Host       string
			Port       uint32
			OriginHost string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &target); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}
		j.mu.Lock()
		j.requested = append(j.requested, net.JoinHostPort(target.Host, fmt.Sprint(target.Port)))
		j.mu.Unlock()

		if !j.allowForward {
			// What sshd says with AllowTcpForwarding no.
			_ = nc.Reject(ssh.Prohibited, "administratively prohibited: open failed")
			continue
		}
		go j.forward(nc)
	}
}

func (j *fakeJumpHost) forward(nc ssh.NewChannel) {
	upstream, err := dialTCP(context.Background(), j.forwardTo)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = upstream.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { _, _ = io.Copy(ch, upstream); _ = ch.Close() }()
	_, _ = io.Copy(upstream, ch)
	_ = upstream.Close()
}

func (j *fakeJumpHost) targets() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.requested...)
}

// serveTCP accepts forever, handing each connection to serve. It returns the
// port, which is what SSHJumpHost.Port takes.
func serveTCP(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := listenTCP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go serve(conn)
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// jumpHarness is a fake jump host in front of a fake node, with a dialer
// relaying through the one to reach the other.
type jumpHarness struct {
	jump   *fakeJumpHost
	node   *fakeNode
	dialer *Dialer
	target Target
}

// The harness leaves the host key unpinned; a test that cares about pinning
// calls JumpOverSSH again with one.
func newJumpHarness(t *testing.T, jump *fakeJumpHost) *jumpHarness {
	t.Helper()
	clientKey := newSigner(t)
	node := &fakeNode{t: t, hostKey: newSigner(t), client: clientKey.PublicKey(), stdins: map[string][]byte{}}
	jump.t = t
	jump.hostKey = newSigner(t)
	jump.forwardTo = net.JoinHostPort("127.0.0.1", serveTCP(t, node.serve))

	port := serveTCP(t, jump.serve)
	dialer := &Dialer{HandshakeTimeout: 5 * time.Second}
	if err := dialer.JumpOverSSH(SSHJumpHost{
		User:        testJumpUser,
		Password:    testJumpPassword,
		Port:        port,
		DialTimeout: 5 * time.Second,
		warnf:       func(string, ...any) {},
	}); err != nil {
		t.Fatal(err)
	}
	return &jumpHarness{
		jump:   jump,
		node:   node,
		dialer: dialer,
		// The jump host is dialed by the target's JumpHost field, so it is
		// the loopback address here rather than a hostname.
		target: Target{JumpHost: "127.0.0.1", Address: "10.0.0.7", User: "dgxc-admin", Signer: clientKey},
	}
}

func TestJumpOverSSHReachesTheMachine(t *testing.T) {
	h := newJumpHarness(t, &fakeJumpHost{offerPassword: true, allowForward: true})

	client, err := h.dialer.Dial(context.Background(), h.target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	out, err := client.Run(context.Background(), "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("Run output %q does not contain the command's output", out)
	}

	// The relay must aim at the machine's sshd, not at the jump host's.
	if got := h.jump.targets(); len(got) != 1 || got[0] != "10.0.0.7:22" {
		t.Errorf("forward requested %v, want [10.0.0.7:22]", got)
	}
}

// A jump host with PasswordAuthentication off but PAM keyboard-interactive on
// is common enough that offering only ssh.Password would fail against it with
// "no supported methods remain", which reads like a wrong password.
func TestJumpOverSSHAnswersKeyboardInteractive(t *testing.T) {
	h := newJumpHarness(t, &fakeJumpHost{offerKeyboard: true, allowForward: true})

	client, err := h.dialer.Dial(context.Background(), h.target)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
}

func TestJumpOverSSHRejectsWrongPassword(t *testing.T) {
	jump := &fakeJumpHost{offerPassword: true, allowForward: true}
	h := newJumpHarness(t, jump)
	if err := h.dialer.JumpOverSSH(SSHJumpHost{
		User:        testJumpUser,
		Password:    "wrong",
		Port:        serveTCP(t, jump.serve),
		DialTimeout: 5 * time.Second,
		warnf:       func(string, ...any) {},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := h.dialer.Dial(context.Background(), h.target)
	if err == nil {
		t.Fatal("dialed with the wrong jump host password")
	}
	if !strings.Contains(err.Error(), testJumpUser+"@") {
		t.Errorf("error %q does not name the account that was refused", err)
	}
}

// A jump host that accepts TCP and never speaks SSH must not hang the relay:
// nothing above it can interrupt a stalled handshake, so an unbounded one
// holds ProvisionNode until the test binary's own backstop kills it.
func TestJumpOverSSHBoundsAStalledHandshake(t *testing.T) {
	stalled := make(chan struct{})
	t.Cleanup(func() { close(stalled) })
	silent := func(conn net.Conn) {
		<-stalled
		_ = conn.Close()
	}
	port := serveTCP(t, silent)

	for name, tc := range map[string]struct {
		timeout time.Duration
		ctx     func() (context.Context, context.CancelFunc)
	}{
		"deadline": {
			timeout: 200 * time.Millisecond,
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
		},
		"context": {
			timeout: time.Minute,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 200*time.Millisecond)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := SSHJumpHost{
				User: testJumpUser, Password: testJumpPassword,
				Port: port, DialTimeout: tc.timeout, warnf: func(string, ...any) {},
			}
			ctx, cancel := tc.ctx()
			defer cancel()

			done := make(chan error, 1)
			go func() {
				conn, err := j.relay(ctx, "127.0.0.1", "10.0.0.7")
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("relay succeeded against a jump host that never answered")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("relay did not return against a stalled jump host")
			}
		})
	}
}

// Forwarding refused is indistinguishable from the machine not listening
// unless the error says so, and the jump host's configuration is the more
// likely cause on a first run.
func TestJumpOverSSHNamesForwardingWhenRefused(t *testing.T) {
	h := newJumpHarness(t, &fakeJumpHost{offerPassword: true})

	_, err := h.dialer.Dial(context.Background(), h.target)
	if err == nil {
		t.Fatal("dialed through a jump host that refuses forwards")
	}
	if !strings.Contains(err.Error(), "AllowTcpForwarding") {
		t.Errorf("error %q does not mention AllowTcpForwarding", err)
	}
}

func TestJumpOverSSHPinsHostKey(t *testing.T) {
	jump := &fakeJumpHost{offerPassword: true, allowForward: true}
	h := newJumpHarness(t, jump)

	pinned := ssh.FingerprintSHA256(jump.hostKey.PublicKey())
	port := serveTCP(t, jump.serve)

	// The real key is accepted.
	if err := h.dialer.JumpOverSSH(SSHJumpHost{
		User: testJumpUser, Password: testJumpPassword, HostKeyFingerprint: pinned,
		Port: port, DialTimeout: 5 * time.Second, warnf: func(string, ...any) {},
	}); err != nil {
		t.Fatal(err)
	}
	client, err := h.dialer.Dial(context.Background(), h.target)
	if err != nil {
		t.Fatalf("dial with the correct pin: %v", err)
	}
	_ = client.Close()

	// Another host's key is not, and the error carries both so the mismatch
	// can be judged rather than guessed at.
	other := ssh.FingerprintSHA256(newSigner(t).PublicKey())
	if err = h.dialer.JumpOverSSH(SSHJumpHost{
		User: testJumpUser, Password: testJumpPassword, HostKeyFingerprint: other,
		Port: port, DialTimeout: 5 * time.Second, warnf: func(string, ...any) {},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = h.dialer.Dial(context.Background(), h.target)
	if err == nil {
		t.Fatal("dialed a jump host whose key does not match the pin")
	}
	if !strings.Contains(err.Error(), pinned) || !strings.Contains(err.Error(), other) {
		t.Errorf("error %q does not carry both the presented and the wanted fingerprint", err)
	}
}

func TestJumpOverSSHWarnsWhenUnpinned(t *testing.T) {
	jump := &fakeJumpHost{offerPassword: true, allowForward: true}
	h := newJumpHarness(t, jump)

	var warnings []string
	if err := h.dialer.JumpOverSSH(SSHJumpHost{
		User: testJumpUser, Password: testJumpPassword,
		Port: serveTCP(t, jump.serve), DialTimeout: 5 * time.Second,
		warnf: func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	}); err != nil {
		t.Fatal(err)
	}
	client, err := h.dialer.Dial(context.Background(), h.target)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], ssh.FingerprintSHA256(jump.hostKey.PublicKey())) {
		t.Errorf("warning %q does not report the key it accepted", warnings[0])
	}
	if !strings.Contains(warnings[0], "jumpHostKeyFingerprint") {
		t.Errorf("warning %q does not say how to pin it", warnings[0])
	}
}

func TestSSHJumpHostValidate(t *testing.T) {
	valid := SSHJumpHost{User: testJumpUser, Password: testJumpPassword}
	for _, tc := range []struct {
		name  string
		jump  SSHJumpHost
		wants string
	}{
		{"complete", valid, ""},
		{"no user", SSHJumpHost{Password: testJumpPassword}, "no user"},
		{"blank user", SSHJumpHost{User: "  ", Password: testJumpPassword}, "no user"},
		{"no password", SSHJumpHost{User: testJumpUser}, "no password"},
		{
			// An MD5 fingerprint, which older ssh-keygen prints. Caught here
			// rather than at the callback, where it would fail every dial as
			// a key mismatch and read as the jump host having changed.
			"md5 fingerprint",
			SSHJumpHost{User: testJumpUser, Password: testJumpPassword, HostKeyFingerprint: "d4:1d:8c:d9:8f:00:b2:04:e9:80:09:98:ec:f8:42:7e"},
			"not a SHA256 fingerprint",
		},
		{
			"sha256 fingerprint without its prefix",
			SSHJumpHost{User: testJumpUser, Password: testJumpPassword, HostKeyFingerprint: strings.TrimPrefix(ssh.FingerprintSHA256(newSigner(t).PublicKey()), fingerprintPrefix)},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.jump.validate()
			switch {
			case tc.wants == "" && err != nil:
				t.Fatalf("validate: %v", err)
			case tc.wants == "":
			case err == nil:
				t.Fatalf("validate accepted it; want %q", tc.wants)
			case !strings.Contains(err.Error(), tc.wants):
				t.Errorf("validate: %v, want it to mention %q", err, tc.wants)
			}
		})
	}
}

func TestJumpOverSSHRejectsAnIncompleteJumpHost(t *testing.T) {
	d := &Dialer{}
	if err := d.JumpOverSSH(SSHJumpHost{User: testJumpUser}); err == nil {
		t.Fatal("accepted a jump host with no password")
	}
	// The relay must be left alone, so a misconfiguration does not silently
	// downgrade a run to Teleport with a half-built SSH route behind it.
	if d.relay != nil {
		t.Error("a rejected jump host replaced the relay")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	const bare = "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{bare, fingerprintPrefix + bare},
		{fingerprintPrefix + bare, fingerprintPrefix + bare},
		{"  " + fingerprintPrefix + bare + "\n", fingerprintPrefix + bare},
	} {
		if got := normalizeFingerprint(tc.in); got != tc.want {
			t.Errorf("normalizeFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Closing the forwarded stream has to close the jump host connection with it.
// A run dials a machine repeatedly; leaking an SSH client per dial would hold
// a session on the jump host for the length of the run.
func TestJumpConnClosesTheJumpHostConnection(t *testing.T) {
	h := newJumpHarness(t, &fakeJumpHost{offerPassword: true, allowForward: true})

	client, err := h.dialer.Dial(context.Background(), h.target)
	if err != nil {
		t.Fatal(err)
	}
	conn, ok := client.stream.(*jumpConn)
	if !ok {
		t.Fatalf("stream is %T, want *jumpConn", client.stream)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	// The client is closed, so a second channel on it must fail.
	if _, err := conn.client.NewSession(); err == nil {
		t.Error("the jump host connection is still open after Close")
	}
}
