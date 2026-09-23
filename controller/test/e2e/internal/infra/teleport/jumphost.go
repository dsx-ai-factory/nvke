// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	fingerprintPrefix   = "SHA256:"
	defaultJumpHostPort = "22"
	defaultJumpTimeout  = 30 * time.Second
)

// SSHJumpHost reaches a jump host over plain SSH with a password, for a site
// whose jump host is not Teleport-enrolled yet. It replaces only the relay:
// the session to the machine beyond it is negotiated exactly as it is over
// tsh, with the key the run generated, so nothing about the machine changes.
//
// The password is the jump host's, and only the jump host's. A machine behind
// it is reached with that key and never with a password, which is why there
// is no field for one here.
type SSHJumpHost struct {
	User     string
	Password string

	// HostKeyFingerprint is the jump host's host key, in the form
	// ssh.FingerprintSHA256 prints ("SHA256:" and 43 base64 characters); the
	// prefix may be omitted. Read it with
	//
	//	ssh-keyscan -t ed25519 <jump-host> | ssh-keygen -lf -
	//
	// Empty accepts whatever key the host presents and says so, loudly, on
	// every dial. Trust-on-first-use is defensible for the machine beyond the
	// jump host, which was created seconds earlier and has no prior key to
	// compare against; it is not defensible for a long-lived jump host that
	// every run authenticates to with a password.
	HostKeyFingerprint string

	// Port is the jump host's SSH port. Empty is 22.
	Port string

	// DialTimeout bounds reaching the jump host and completing its
	// handshake. Zero is 30s.
	DialTimeout time.Duration

	// warnf receives the unpinned-host-key warning. Nil writes to stderr,
	// which is where a Ginkgo run collects it.
	warnf func(format string, args ...any)
}

// JumpOverSSH makes the dialer relay through the jump host over plain SSH
// rather than tsh. Teleport stays the default: this is called only when a
// jump host password is supplied, so configuring Teleport later is a matter
// of removing that secret rather than changing any code.
func (d *Dialer) JumpOverSSH(j SSHJumpHost) error {
	if err := j.validate(); err != nil {
		return fmt.Errorf("ssh jump host: %w", err)
	}
	d.relay = j.relay
	return nil
}

func (j SSHJumpHost) validate() error {
	var problems []string
	if strings.TrimSpace(j.User) == "" {
		problems = append(problems, "no user")
	}
	if j.Password == "" {
		problems = append(problems, "no password")
	}
	if fp := normalizeFingerprint(j.HostKeyFingerprint); fp != "" {
		// Rejected here rather than at the callback, because a malformed pin
		// would otherwise fail every dial as a key mismatch and read as the
		// jump host having changed.
		if len(fp) != len(fingerprintPrefix)+43 {
			problems = append(problems, fmt.Sprintf(
				"host key fingerprint %q is not a SHA256 fingerprint; "+
					"read one with `ssh-keyscan -t ed25519 <host> | ssh-keygen -lf -`", j.HostKeyFingerprint))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// relay opens a direct-tcpip channel to the machine's sshd. The jump host's
// own sshd does the forwarding, so unlike the tsh relay this needs neither nc
// nor bash's /dev/tcp there -- but it does need AllowTcpForwarding, which the
// error below names because a refusal is otherwise indistinguishable from the
// machine not listening yet.
func (j SSHJumpHost) relay(ctx context.Context, jumpHost, address string) (net.Conn, error) {
	addr := net.JoinHostPort(jumpHost, j.port())
	dialer := net.Dialer{Timeout: j.timeout()}
	tcp, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dialing jump host %s: %w", addr, err)
	}

	cfg := &ssh.ClientConfig{
		User:            j.User,
		Auth:            j.authMethods(),
		HostKeyCallback: j.hostKeyCallback(),
		Timeout:         j.timeout(),
	}
	conn, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("ssh to %s@%s: %w", j.User, addr, err)
	}
	client := ssh.NewClient(conn, chans, reqs)

	// Not the dial's context: the stream has to outlive the dial that opened
	// it, and Close is what ends it. Same contract as the tsh relay.
	stream, err := client.DialContext(context.WithoutCancel(ctx), "tcp", net.JoinHostPort(address, sshPort))
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("forwarding through %s to %s:%s: %w "+
			"(the jump host may have AllowTcpForwarding off, or the machine is not listening yet)",
			jumpHost, address, sshPort, err)
	}
	return &jumpConn{Conn: stream, client: client}, nil
}

// authMethods offers the password twice. Plenty of sshd configurations answer
// PasswordAuthentication no while still serving passwords through
// keyboard-interactive (PAM), and offering only the first fails against them
// with "no supported methods remain", which reads like a wrong password.
func (j SSHJumpHost) authMethods() []ssh.AuthMethod {
	answer := func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = j.Password
		}
		return answers, nil
	}
	return []ssh.AuthMethod{ssh.Password(j.Password), ssh.KeyboardInteractive(answer)}
}

func (j SSHJumpHost) hostKeyCallback() ssh.HostKeyCallback {
	want := normalizeFingerprint(j.HostKeyFingerprint)
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		if want == "" {
			j.warningf("jump host %s presented host key %s, which was accepted UNCHECKED "+
				"because no fingerprint is configured. Pin it by setting "+
				"site.jumpHostKeyFingerprint to that value.", hostname, got)
			return nil
		}
		if got != want {
			return fmt.Errorf("jump host %s presented host key %s, want %s", hostname, got, want)
		}
		return nil
	}
}

// warningf is not warnf, which is the field it falls back from: a method
// cannot share a name with a field.
func (j SSHJumpHost) warningf(format string, args ...any) {
	if j.warnf != nil {
		j.warnf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: "+format+"\n", args...)
}

func (j SSHJumpHost) port() string {
	if j.Port == "" {
		return defaultJumpHostPort
	}
	return j.Port
}

func (j SSHJumpHost) timeout() time.Duration {
	if j.DialTimeout == 0 {
		return defaultJumpTimeout
	}
	return j.DialTimeout
}

// normalizeFingerprint accepts what `ssh-keygen -lf` prints, with or without
// the SHA256: prefix, so a value copied from either tool works.
func normalizeFingerprint(fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ""
	}
	if strings.HasPrefix(fingerprint, fingerprintPrefix) {
		return fingerprint
	}
	return fingerprintPrefix + fingerprint
}

// jumpConn is the forwarded stream, holding the jump host connection open for
// as long as it lasts. Closing the stream alone would leave an idle SSH
// client and its TCP connection behind for every machine a run dials.
type jumpConn struct {
	net.Conn
	client *ssh.Client
	once   sync.Once
}

func (c *jumpConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		_ = c.client.Close()
	})
	return err
}
