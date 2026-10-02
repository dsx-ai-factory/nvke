// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package teleport reaches a machine that is not enrolled in Teleport through
// a jump host that is, or -- until a site's jump host is enrolled -- through
// one reached over plain SSH. See SSHJumpHost for that interim path; only the
// relay differs between them.
//
// The jump host only relays bytes. The SSH session to the machine is
// negotiated here, with a key that never leaves the runner, so the jump host
// needs no credential for the machine and Teleport's port-forwarding policy
// does not apply: forwards ride the machine's own sshd.
package teleport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	defaultHandshakeTimeout = 30 * time.Second
	sshPort                 = "22"
)

// Target is a machine reached through a jump host, as a user authenticating
// with a key.
type Target struct {
	JumpHost string
	Address  string
	User     string
	Signer   ssh.Signer
}

func (t Target) validate() error {
	switch {
	case strings.TrimSpace(t.JumpHost) == "":
		return errors.New("no jump host")
	case net.ParseIP(t.Address) == nil:
		// Also what keeps the address out of reach of the relay's shell.
		return fmt.Errorf("address %q is not an IP", t.Address)
	case strings.TrimSpace(t.User) == "":
		return errors.New("no user")
	case t.Signer == nil:
		return errors.New("no key")
	}
	return nil
}

// Dialer opens SSH connections through Teleport jump hosts with tsh.
type Dialer struct {
	// Proxy is the Teleport proxy. Empty leaves it to tsh: $TELEPORT_PROXY,
	// else the logged-in profile's.
	Proxy string
	// IdentityFile is a machine identity, which is what CI has. Empty leaves
	// it to tsh: $TELEPORT_IDENTITY_FILE, else the logged-in profile.
	IdentityFile string
	// Login is the jump host login. Empty is tsh's default.
	Login string
	// TSH is the tsh binary. Empty is tsh on PATH.
	TSH string

	HandshakeTimeout time.Duration

	// relay opens the byte stream to address's sshd; nil is tsh.
	relay func(ctx context.Context, jumpHost, address string) (net.Conn, error)

	mu       sync.Mutex
	hostKeys map[string][]byte
}

// Dial connects to target and completes an SSH handshake with it. It makes
// one attempt: a machine that is still booting fails fast, and retrying is
// the caller's decision.
func (d *Dialer) Dial(ctx context.Context, target Target) (*Client, error) {
	if err := target.validate(); err != nil {
		return nil, fmt.Errorf("teleport: %w", err)
	}
	relay := d.relay
	if relay == nil {
		relay = d.tshRelay
	}
	stream, err := relay(ctx, target.JumpHost, target.Address)
	if err != nil {
		return nil, fmt.Errorf("teleport: relaying to %s through %s: %w", target.Address, target.JumpHost, err)
	}

	hostKey, pin := d.pinHostKey(target)
	cfg := &ssh.ClientConfig{
		User:            target.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(target.Signer)},
		HostKeyCallback: hostKey,
	}
	type handshake struct {
		conn  ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		err   error
	}
	done := make(chan handshake, 1)
	go func() {
		conn, chans, reqs, err := ssh.NewClientConn(stream, net.JoinHostPort(target.Address, sshPort), cfg)
		done <- handshake{conn, chans, reqs, err}
	}()

	timeout := d.HandshakeTimeout
	if timeout == 0 {
		timeout = defaultHandshakeTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	fail := func(err error) error {
		_ = stream.Close()
		<-done
		return fmt.Errorf("teleport: ssh to %s@%s through %s: %w%s", target.User, target.Address, target.JumpHost, err, relayDiagnostics(stream))
	}
	select {
	case hs := <-done:
		if hs.err != nil {
			_ = stream.Close()
			return nil, fmt.Errorf("teleport: ssh to %s@%s through %s: %w%s", target.User, target.Address, target.JumpHost, hs.err, relayDiagnostics(stream))
		}
		pin()
		return &Client{ssh: ssh.NewClient(hs.conn, hs.chans, hs.reqs), stream: stream}, nil
	case <-ctx.Done():
		return nil, fail(ctx.Err())
	case <-timer.C:
		return nil, fail(fmt.Errorf("no handshake after %s", timeout))
	}
}

// pinHostKey trusts the host key of the first dial that succeeds and holds a
// machine to that key on every later dial. There is nothing better to check
// against: the machine was created moments ago and its key generated on first
// boot. The pin is per target key as well as address, because a later machine
// in the same run can be handed the same address.
//
// The pin is committed by the returned func, not by the callback, and callers
// must only call it once the whole handshake has succeeded. A caller dialing a
// machine that is still being imaged reaches several different sshds on the
// way -- whatever answers on a recycled address, then the imager, then the
// installed OS -- and pinning the first key any of them presents locks the
// machine out of its own run. Authentication is the signal that the right one
// answered: the key is generated per run and placed by cloud-init, so only the
// machine that ran this run's cloud-init can complete a handshake.
func (d *Dialer) pinHostKey(target Target) (ssh.HostKeyCallback, func()) {
	id := target.Address + " " + ssh.FingerprintSHA256(target.Signer.PublicKey())
	var offered []byte
	callback := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		offered = key.Marshal()
		known, ok := d.hostKeys[id]
		if ok && !bytes.Equal(known, offered) {
			return fmt.Errorf("host key for %s changed since the first connection (now %s)", target.Address, ssh.FingerprintSHA256(key))
		}
		return nil
	}
	commit := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if _, ok := d.hostKeys[id]; ok || offered == nil {
			return
		}
		if d.hostKeys == nil {
			d.hostKeys = map[string][]byte{}
		}
		d.hostKeys[id] = offered
	}
	return callback, commit
}

func relayDiagnostics(stream net.Conn) string {
	diag, ok := stream.(interface{ Diagnostics() string })
	if !ok {
		return ""
	}
	if out := strings.TrimSpace(diag.Diagnostics()); out != "" {
		return "\nrelay: " + out
	}
	return ""
}
