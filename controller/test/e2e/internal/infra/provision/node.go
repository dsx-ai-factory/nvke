// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package provision carries the half of building a site-controller node that
// does not depend on who built the machine.
//
// A provider creates an instance on its own infrastructure, in its own
// vocabulary, through its own API. Everything after that is the same: dial the
// node through its jump host until sshd answers, wait for cloud-init to settle
// and read whether it is fit to bootstrap on, and release the machine when the
// run ends or fails part-way. That is what lives here.
//
// It is deliberately not an interface a provider implements. The pieces are
// called directly, in the order a provider's own ProvisionNode lays out, so
// that reading one provider tells you what it does rather than sending you to
// a framework.
package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/teleport"
)

const (
	// cloudInitDiagnosticTimeout bounds the diagnostic collection. The context
	// that reached this point may already be at its deadline, and a node that
	// has stopped answering must not turn a reported failure into a hang.
	cloudInitDiagnosticTimeout = 2 * time.Minute

	// cloudInitStatusError is the status cloud-init reports when a module
	// failed outright, as opposed to the "degraded done" it reports for a
	// warning.
	cloudInitStatusError = "error"

	// ReleaseTimeout bounds a delete issued when provisioning fails part-way,
	// or when a session is torn down. Both run detached from the caller's
	// context, so they need a bound of their own.
	ReleaseTimeout = 5 * time.Minute
)

// Images resolves the Node version under qualification to the two URLs the
// iPXE script boots: the qcow imager and the image it writes. Both must be
// plain HTTPS, since iPXE cannot pull an OCI artifact.
//
// The architecture is a parameter because a run can span two: the machine
// being created decides which image it needs, and only the provider knows
// which machine that is.
type Images func(nodeVersion, arch string) (bootLoaderURL, imageURL string, err error)

// Conn is a connection to a node. It is everything a Session does except
// release the machine, which is the provider's.
type Conn interface {
	Run(ctx context.Context, cmd string) (string, error)
	Put(ctx context.Context, path string, data []byte, mode uint32) error
	Fetch(ctx context.Context, path string) ([]byte, error)
	Forward(ctx context.Context, remote string) (string, error)
	Close() error
}

// Dialer opens a connection to a node through its site's jump host. A dial
// that fails because the node is not answering yet is retried by the caller,
// so a Dialer makes one attempt and reports what happened.
type Dialer interface {
	Dial(ctx context.Context, target teleport.Target) (Conn, error)
}

// Teleport is the Dialer a real run uses.
func Teleport(d *teleport.Dialer) Dialer { return teleportDialer{d} }

type teleportDialer struct{ d *teleport.Dialer }

func (t teleportDialer) Dial(ctx context.Context, target teleport.Target) (Conn, error) {
	c, err := t.d.Dial(ctx, target)
	if err != nil {
		// Not c: a nil *Client in a Conn is a non-nil Conn.
		return nil, err
	}
	return c, nil
}

// WaitReachable dials until the node answers.
//
// Ready and reachable are different signals, and on every substrate seen so
// far the first arrives long before the second: an instance is reported ready
// once its provider has handed it over, which is before the image is written,
// booted and running sshd.
func WaitReachable(ctx context.Context, dialer Dialer, target teleport.Target, provider string, reach, poll time.Duration) (Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, reach)
	defer cancel()

	var lastErr error
	for {
		conn, err := dialer.Dial(ctx, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: %s not reachable through %s after %s: %w", provider, target.Address, target.JumpHost, reach, lastErr)
		case <-time.After(poll):
		}
	}
}

// cloudInitReport is the part of `cloud-init status --format=json` the node
// preflight reads.
type cloudInitReport struct {
	Status         string   `json:"status"`
	ExtendedStatus string   `json:"extended_status"`
	Errors         []string `json:"errors"`
	// RecoverableErrors are warnings. They set "degraded done" and a non-zero
	// exit code without saying anything about whether the user data ran, so
	// they are reported when something else fails rather than failing a node
	// themselves.
	RecoverableErrors map[string][]string `json:"recoverable_errors"`
}

// AwaitCloudInit blocks until cloud-init settles, then reports whether the
// node is fit to bootstrap on.
//
// The exit code of `cloud-init status` is not that verdict. An instance is
// created with phone-home disabled and cannot reach the metadata service, so
// cloud-init finds no datasource, falls back to DataSourceNone and finishes
// "degraded done" -- exit 2 -- over a warning that says nothing about whether
// the user data ran. `errors` is what carries a real failure, so that is what
// the preflight reads.
func AwaitCloudInit(ctx context.Context, conn Conn, provider, name string) error {
	// The wait must not decide the outcome, so its exit code is discarded and
	// the verdict comes from the report below.
	if _, err := conn.Run(ctx, "cloud-init status --wait >/dev/null 2>&1 || true"); err != nil {
		return fmt.Errorf("%s: waiting for cloud-init on %s: %w", provider, name, err)
	}

	// stderr discarded, not merged: Run returns combined output, and a
	// warning on stderr would be spliced into the JSON and fail the parse,
	// rejecting a node that is fine.
	out, err := conn.Run(ctx, "cloud-init status --format=json 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("%s: reading cloud-init status on %s: %w", provider, name, err)
	}

	var report cloudInitReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return fmt.Errorf("%s: cloud-init status on %s is not the JSON the preflight reads: %w\n%s%s", provider, name, err, out, CloudInitDiagnostics(ctx, conn))
	}
	if report.Status == cloudInitStatusError || len(report.Errors) > 0 {
		detail := strings.Join(report.Errors, "; ")
		if detail == "" {
			detail = report.ExtendedStatus
		}
		return fmt.Errorf("%s: cloud-init on %s failed, and node preflight would reject it: %s%s", provider, name, detail, CloudInitDiagnostics(ctx, conn))
	}
	return nil
}

// CloudInitDiagnostics reports why cloud-init degraded. `cloud-init status
// --wait` gives an exit code and the word "done", which is not enough to act
// on once the run has deleted the instance.
//
// Best effort throughout: a probe that fails contributes its error rather
// than replacing the failure being reported, since the caller already has the
// only fact that must survive.
func CloudInitDiagnostics(ctx context.Context, conn Conn) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudInitDiagnosticTimeout)
	defer cancel()

	probes := []struct{ label, cmd string }{
		// --long carries the recoverable errors that set the exit code.
		{"cloud-init status --long", "cloud-init status --long"},
		// Where a failing runcmd prints. The usual answer is here.
		{"cloud-init-output.log (tail)", "sudo -n tail -n 120 /var/log/cloud-init-output.log"},
		// cloud-init's own warnings, for a module that failed before runcmd.
		{"cloud-init.log (warnings)", "sudo -n grep -aE 'WARNING|ERROR|CRITICAL' /var/log/cloud-init.log | tail -n 40"},
	}

	var b strings.Builder
	for _, probe := range probes {
		out, err := conn.Run(ctx, probe.cmd)
		b.WriteString("\n--- " + probe.label + " ---\n")
		if err != nil {
			b.WriteString("collecting failed: " + err.Error() + "\n")
		}
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			b.WriteString(trimmed + "\n")
		}
	}
	return b.String()
}

// NewKey generates the key a run authenticates to its node with. It is made
// per run and never stored: cloud-init writes the public half into
// authorized_keys, and the private half does not leave the process.
func NewKey(provider string) (ssh.Signer, string, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("%s: generating the session key: %w", provider, err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, "", fmt.Errorf("%s: generating the session key: %w", provider, err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

// Session is a Conn plus the release of the machine it is connected to.
type Session struct {
	Conn
	// Release returns the machine. It is the provider's, because only the
	// provider knows which API built it.
	Release  func(context.Context) error
	released bool
}

func (s *Session) Teardown(ctx context.Context) error {
	if s.released {
		return nil
	}
	closeErr := s.Close()
	// Detached for the same reason as a failed provision: teardown is called
	// most often from a run that is already ending.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
	defer cancel()
	if err := s.Release(rctx); err != nil {
		return errors.Join(closeErr, err)
	}
	s.released = true
	return closeErr
}

// WithTimeout copies c with at least the given timeout, raising a shorter one
// and leaving an absent one absent. A copy rather than a mutation: the caller
// keeps its client, and a test that injected a transport keeps that too.
func WithTimeout(c *http.Client, timeout time.Duration) *http.Client {
	if c == nil {
		return &http.Client{Timeout: timeout}
	}
	// Zero is "no timeout", not "the shortest one": a caller that left it
	// unset wants the request bounded by its context alone, and raising it to
	// `timeout` would be lowering it from infinity.
	if c.Timeout == 0 || c.Timeout >= timeout {
		return c
	}
	clone := *c
	clone.Timeout = timeout
	return &clone
}
