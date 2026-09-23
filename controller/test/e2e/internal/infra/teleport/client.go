// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Client is an SSH connection to a machine behind a jump host. Put and Fetch
// act as root through sudo, which the machine's user must hold without a
// password.
type Client struct {
	ssh    *ssh.Client
	stream net.Conn

	mu        sync.Mutex
	listeners []net.Listener
	closed    bool
}

// Run executes cmd and fails on a non-zero exit, returning the combined
// output either way.
func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	var out lockedBuffer
	err := c.exec(ctx, cmd, nil, &out, &out)
	return out.String(), err
}

// Put writes data to p with mode. It writes beside p and renames, so a
// transfer cut short never leaves a truncated file where a binary is
// expected.
func (c *Client) Put(ctx context.Context, p string, data []byte, mode uint32) error {
	if err := checkPath(p); err != nil {
		return err
	}
	tmp := p + ".nke-e2e-partial"
	script := fmt.Sprintf("umask 077 && mkdir -p %s && cat > %s && chmod %s %s && mv -f %s %s",
		shellQuote(path.Dir(p)), shellQuote(tmp), strconv.FormatUint(uint64(mode&0o7777), 8), shellQuote(tmp), shellQuote(tmp), shellQuote(p))
	var stderr lockedBuffer
	if err := c.exec(ctx, "sudo -n sh -c "+shellQuote(script), bytes.NewReader(data), io.Discard, &stderr); err != nil {
		return fmt.Errorf("teleport: writing %s: %w%s", p, err, suffix(stderr.String()))
	}
	return nil
}

// Fetch reads p.
func (c *Client) Fetch(ctx context.Context, p string) ([]byte, error) {
	if err := checkPath(p); err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	var stderr lockedBuffer
	if err := c.exec(ctx, "sudo -n cat "+shellQuote(p), nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("teleport: reading %s: %w%s", p, err, suffix(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Forward listens on a local port and carries each connection to remotePort
// on the machine's loopback. It checks the port answers before returning, so
// a forward to nothing fails here rather than at first use. The forward
// lives until Close.
func (c *Client) Forward(ctx context.Context, remotePort int) (string, error) {
	if remotePort <= 0 || remotePort > 65535 {
		return "", fmt.Errorf("teleport: port %d out of range", remotePort)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	remote := net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort))
	probe, err := c.ssh.DialContext(ctx, "tcp", remote)
	if err != nil {
		return "", fmt.Errorf("teleport: nothing answers on %s on the machine: %w", remote, err)
	}
	_ = probe.Close()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("teleport: listening for a forward: %w", err)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = ln.Close()
		return "", errors.New("teleport: client is closed")
	}
	c.listeners = append(c.listeners, ln)
	c.mu.Unlock()

	go func() {
		for {
			local, err := ln.Accept()
			if err != nil {
				return
			}
			go c.carry(local, remote)
		}
	}()
	return ln.Addr().String(), nil
}

func (c *Client) carry(local net.Conn, remote string) {
	defer func() { _ = local.Close() }()
	upstream, err := c.ssh.Dial("tcp", remote)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, upstream); done <- struct{}{} }()
	<-done
}

// Close ends every forward and the connection. It is safe to call more than
// once.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	listeners := c.listeners
	c.listeners = nil
	c.mu.Unlock()

	for _, ln := range listeners {
		_ = ln.Close()
	}
	err := c.ssh.Close()
	_ = c.stream.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// exec runs cmd in a new session. A canceled context kills the command
// rather than waiting on it, since a bootstrap step can run for many minutes.
func (c *Client) exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	session, err := c.ssh.NewSession()
	if err != nil {
		return fmt.Errorf("teleport: opening a session: %w", err)
	}
	defer func() { _ = session.Close() }()
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		<-done
		return ctx.Err()
	}
}

func checkPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("teleport: %q is not a clean absolute path", p)
	}
	return nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func suffix(stderr string) string {
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		return ": " + stderr
	}
	return ""
}

// lockedBuffer takes stdout and stderr together: the SSH library writes each
// from its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
