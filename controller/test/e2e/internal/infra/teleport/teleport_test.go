// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// fakeNode is an sshd that accepts one client key, answers a handful of
// commands, and forwards one port to a local listener.
type fakeNode struct {
	t       *testing.T
	hostKey ssh.Signer
	client  ssh.PublicKey

	forwardPort int    // the port the node "has" open on loopback
	forwardTo   string // where connections to it actually go

	mu     sync.Mutex
	stdins map[string][]byte
}

func (n *fakeNode) serve(conn net.Conn) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), n.client.Marshal()) {
				return nil, io.EOF
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(n.hostKey)
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sconn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go n.session(nc)
		case "direct-tcpip":
			go n.forward(nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

func (n *fakeNode) session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		cmd := string(req.Payload[4:])
		_ = req.Reply(true, nil)
		status := n.exec(cmd, ch, reqs)
		_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
		return
	}
}

func (n *fakeNode) exec(cmd string, ch ssh.Channel, reqs <-chan *ssh.Request) uint32 {
	switch {
	case cmd == "echo hi":
		_, _ = io.WriteString(ch, "hi\n")
		_, _ = io.WriteString(ch.Stderr(), "and stderr\n")
		return 0
	case cmd == "fail":
		_, _ = io.WriteString(ch.Stderr(), "boom\n")
		return 3
	case cmd == "hang":
		for req := range reqs {
			if req.Type == "signal" {
				return 137
			}
		}
		return 137
	case strings.HasPrefix(cmd, "sudo -n sh -c "):
		data, _ := io.ReadAll(ch)
		n.mu.Lock()
		n.stdins[cmd] = data
		n.mu.Unlock()
		return 0
	case cmd == "sudo -n cat '/etc/kubernetes/admin.conf'":
		_, _ = io.WriteString(ch, "kubeconfig")
		return 0
	case strings.HasPrefix(cmd, "sudo -n cat "):
		_, _ = io.WriteString(ch.Stderr(), "cat: No such file or directory\n")
		return 1
	}
	n.t.Errorf("unexpected command %q", cmd)
	return 127
}

func (n *fakeNode) forward(nc ssh.NewChannel) {
	var target struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &target); err != nil || target.Host != "127.0.0.1" || int(target.Port) != n.forwardPort {
		_ = nc.Reject(ssh.ConnectionFailed, "connection refused")
		return
	}
	upstream, err := dialTCP(context.Background(), n.forwardTo)
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

type harness struct {
	node   *fakeNode
	dialer *Dialer
	target Target
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	echo, err := listenTCP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	client := newSigner(t)
	h := &harness{
		node: &fakeNode{
			t: t, hostKey: newSigner(t), client: client.PublicKey(),
			forwardPort: 6443, forwardTo: echo.Addr().String(),
			stdins: map[string][]byte{},
		},
		target: Target{JumpHost: "jump", Address: "10.0.0.7", User: "dgxc-admin", Signer: client},
	}
	h.dialer = &Dialer{HandshakeTimeout: 5 * time.Second, relay: func(ctx context.Context, jumpHost, address string) (net.Conn, error) {
		if jumpHost != "jump" || address != "10.0.0.7" {
			t.Errorf("relay to %s through %s", address, jumpHost)
		}
		return connectedPair(ctx, t, h.node.serve)
	}}
	return h
}

func listenTCP(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", "127.0.0.1:0")
}

func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// connectedPair is a buffered stream to serve, as a relay is. net.Pipe will
// not do: both ends of an SSH handshake write before they read, and an
// unbuffered pipe deadlocks on that.
func connectedPair(ctx context.Context, t *testing.T, serve func(net.Conn)) (net.Conn, error) {
	t.Helper()
	ln, err := listenTCP(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, acceptErr := ln.Accept()
		if acceptErr != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	near, err := dialTCP(ctx, ln.Addr().String())
	if err != nil {
		return nil, err
	}
	far, ok := <-accepted
	if !ok {
		_ = near.Close()
		return nil, io.EOF
	}
	t.Cleanup(func() { _ = far.Close() })
	go serve(far)
	return near, nil
}

func (h *harness) dial(t *testing.T) *Client {
	t.Helper()
	c, err := h.dialer.Dial(context.Background(), h.target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRun(t *testing.T) {
	c := newHarness(t).dial(t)

	out, err := c.Run(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "hi\n") || !strings.Contains(out, "and stderr") {
		t.Errorf("output = %q, want stdout and stderr combined", out)
	}

	out, err = c.Run(context.Background(), "fail")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("output = %q, want what the node said on failure", out)
	}
}

func TestRunCanceled(t *testing.T) {
	c := newHarness(t).dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Run(ctx, "hang"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a canceled command should not be waited on")
	}
}

func TestPut(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t)

	if err := c.Put(context.Background(), "/usr/local/bin/nkectl", []byte("binary"), 0o755); err != nil {
		t.Fatalf("put: %v", err)
	}
	h.node.mu.Lock()
	defer h.node.mu.Unlock()
	if len(h.node.stdins) != 1 {
		t.Fatalf("ran %d writes, want one", len(h.node.stdins))
	}
	for cmd, data := range h.node.stdins {
		if string(data) != "binary" {
			t.Errorf("stdin = %q, want the data", data)
		}
		for _, want := range []string{"chmod 755", "/usr/local/bin/nkectl.nke-e2e-partial", "mv -f", "mkdir -p '\\''/usr/local/bin'\\''"} {
			if !strings.Contains(cmd, want) {
				t.Errorf("write command %q should contain %q", cmd, want)
			}
		}
	}
}

func TestPutAndFetchRejectUncleanPaths(t *testing.T) {
	c := newHarness(t).dial(t)
	for _, p := range []string{"relative", "/etc/../etc/passwd", "/tmp/"} {
		if err := c.Put(context.Background(), p, nil, 0o600); err == nil {
			t.Errorf("Put(%q) should fail", p)
		}
		if _, err := c.Fetch(context.Background(), p); err == nil {
			t.Errorf("Fetch(%q) should fail", p)
		}
	}
}

func TestFetch(t *testing.T) {
	c := newHarness(t).dial(t)

	data, err := c.Fetch(context.Background(), "/etc/kubernetes/admin.conf")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(data) != "kubeconfig" {
		t.Errorf("data = %q", data)
	}

	_, err = c.Fetch(context.Background(), "/missing")
	if err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Errorf("err = %v, want the node's reason", err)
	}
}

func TestForward(t *testing.T) {
	c := newHarness(t).dial(t)

	if _, err := c.Forward(context.Background(), 9999); err == nil {
		t.Error("a forward to a closed port should fail when it is set up")
	}

	addr, err := c.Forward(context.Background(), 6443)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	conn, err := dialTCP(context.Background(), addr)
	if err != nil {
		t.Fatalf("dialing the forward: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("read %q, %v; want the echo through the forward", buf, err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if conn, err := dialTCP(context.Background(), addr); err == nil {
		_ = conn.Close()
		t.Error("the forward should stop listening on Close")
	}
}

func TestDialRejectsWrongKey(t *testing.T) {
	h := newHarness(t)
	h.target.Signer = newSigner(t)
	if _, err := h.dialer.Dial(context.Background(), h.target); err == nil {
		t.Fatal("a key the node does not authorize should fail")
	}
}

func TestDialPinsHostKey(t *testing.T) {
	h := newHarness(t)
	_ = h.dial(t)
	_ = h.dial(t)

	h.node.hostKey = newSigner(t)
	_, err := h.dialer.Dial(context.Background(), h.target)
	if err == nil || !strings.Contains(err.Error(), "host key") {
		t.Errorf("err = %v, want a changed host key refused", err)
	}

	// A later machine given the same address has its own client key.
	h.target.Signer = newSigner(t)
	h.node.client = h.target.Signer.PublicKey()
	_ = h.dial(t)
}

func TestDialTimesOut(t *testing.T) {
	d := &Dialer{
		HandshakeTimeout: 50 * time.Millisecond,
		relay: func(context.Context, string, string) (net.Conn, error) {
			near, _ := net.Pipe()
			return near, nil
		},
	}
	target := Target{JumpHost: "jump", Address: "10.0.0.7", User: "u", Signer: newSigner(t)}
	if _, err := d.Dial(context.Background(), target); err == nil || !strings.Contains(err.Error(), "no handshake") {
		t.Errorf("err = %v, want a handshake timeout", err)
	}
}

func TestDialValidatesTarget(t *testing.T) {
	d := &Dialer{relay: func(context.Context, string, string) (net.Conn, error) {
		t.Error("an invalid target must not reach the relay")
		return nil, io.EOF
	}}
	signer := newSigner(t)
	for name, target := range map[string]Target{
		"no jump host":  {Address: "10.0.0.7", User: "u", Signer: signer},
		"hostname":      {JumpHost: "j", Address: "node.example", User: "u", Signer: signer},
		"shell in addr": {JumpHost: "j", Address: "10.0.0.7; reboot", User: "u", Signer: signer},
		"no user":       {JumpHost: "j", Address: "10.0.0.7", Signer: signer},
		"no key":        {JumpHost: "j", Address: "10.0.0.7", User: "u"},
	} {
		if _, err := d.Dial(context.Background(), target); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTSHArgs(t *testing.T) {
	d := &Dialer{Proxy: "teleport.example:443", IdentityFile: "/run/id", Login: "root"}
	got := d.tshArgs("jump.example", "relay")
	want := []string{"ssh", "--proxy", "teleport.example:443", "--identity", "/run/id", "root@jump.example", "relay"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if got := (&Dialer{}).tshArgs("jump", "relay"); !slices.Equal(got, []string{"ssh", "jump", "relay"}) {
		t.Errorf("args with the profile's defaults = %q", got)
	}
}

func TestRelayCommandReachesPort22(t *testing.T) {
	cmd := relayCommand("10.0.0.7")
	for _, want := range []string{"exec nc 10.0.0.7 22", "/dev/tcp/10.0.0.7/22"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("relay %q should contain %q", cmd, want)
		}
	}
}

func TestProcessConn(t *testing.T) {
	conn, err := startRelay(exec.CommandContext(context.Background(), "sh", "-c", "echo refused >&2; exec cat"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := io.WriteString(conn, "bytes"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "bytes" {
		t.Errorf("read %q, %v; want the process's stdout", buf, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := conn.Diagnostics(); strings.TrimSpace(got) != "refused" {
		t.Errorf("diagnostics = %q, want the process's stderr", got)
	}
}

func TestBoundedBuffer(t *testing.T) {
	var b boundedBuffer
	chunk := strings.Repeat("x", 1000)
	for range 20 {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write = %d, %v; a full buffer must not stall the process", n, err)
		}
	}
	if got := len(b.String()); got != diagnosticsLimit {
		t.Errorf("kept %s bytes, want %d", strconv.Itoa(got), diagnosticsLimit)
	}
}
