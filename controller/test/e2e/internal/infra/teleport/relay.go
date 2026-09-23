// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

const (
	// diagnosticsLimit bounds what is kept of the relay's stderr. It is
	// tsh's and the relay's complaints, not session traffic.
	diagnosticsLimit = 8 << 10
)

// relayCommand runs on the jump host and joins its stdio to address's sshd.
// nc is not guaranteed there, so bash's /dev/tcp stands in; bash is, since it
// is the jump host's login shell. address has been checked to be an IP, which
// is what makes interpolating it safe.
func relayCommand(address string) string {
	const script = `if command -v nc >/dev/null 2>&1; then exec nc %[1]s %[2]s; fi; ` +
		`exec bash -c 'exec 3<>/dev/tcp/%[1]s/%[2]s && { cat <&3 & exec cat >&3; }'`
	return fmt.Sprintf(script, address, sshPort)
}

func (d *Dialer) tshArgs(jumpHost, command string) []string {
	args := []string{"ssh"}
	if d.Proxy != "" {
		args = append(args, "--proxy", d.Proxy)
	}
	if d.IdentityFile != "" {
		args = append(args, "--identity", d.IdentityFile)
	}
	host := jumpHost
	if d.Login != "" {
		host = d.Login + "@" + jumpHost
	}
	return append(args, host, command)
}

// tshRelay starts tsh and returns its stdio as the stream. The process is
// detached from the dial's cancellation: the stream has to outlive the dial,
// and Close is what ends it.
func (d *Dialer) tshRelay(ctx context.Context, jumpHost, address string) (net.Conn, error) {
	binary := d.TSH
	if binary == "" {
		binary = "tsh"
	}
	return startRelay(exec.CommandContext(context.WithoutCancel(ctx), binary, d.tshArgs(jumpHost, relayCommand(address))...))
}

func startRelay(cmd *exec.Cmd) (*processConn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	conn := &processConn{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}
	cmd.Stderr = &conn.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		close(conn.exited)
	}()
	return conn, nil
}

// processConn is a subprocess's stdio as a net.Conn. SSH needs nothing more:
// it sets no deadlines and reads no addresses.
type processConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr boundedBuffer
	exited chan struct{}
	once   sync.Once
}

func (c *processConn) Read(b []byte) (int, error)  { return c.stdout.Read(b) }
func (c *processConn) Write(b []byte) (int, error) { return c.stdin.Write(b) }

func (c *processConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		select {
		case <-c.exited:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	})
	return nil
}

// Diagnostics is what the relay wrote to stderr: for a machine still
// booting, the connection refusal.
func (c *processConn) Diagnostics() string { return c.stderr.String() }

func (c *processConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (c *processConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (c *processConn) SetDeadline(time.Time) error      { return nil }
func (c *processConn) SetReadDeadline(time.Time) error  { return nil }
func (c *processConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "tsh" }

type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := diagnosticsLimit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
