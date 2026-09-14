// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nkectl drives the nkectl binary against the NKE deployment the e2e
// suite already has running, so one table entry can prove the command line
// provisions a tenant cluster rather than only that the REST API does.
//
// It is deliberately kept apart from internal/nke. That package is the REST
// client every other spec uses; this one shells out to a binary. Both build the
// same request through nke.BuildCreateRequest, so an entry driven from here
// differs from its REST counterpart in transport and nothing else -- which is
// what makes a failure here legible as a CLI regression.
//
// nkectl cannot borrow the suite's port-forward. kube.ServiceForwarder routes
// through a custom DialContext against a synthetic .svc authority that never
// resolves, so it is unusable from another process. This package runs its own
// `kubectl port-forward` and points a throwaway site config at it, the same
// shape hack/ci/nkectl-smoke.sh uses.
package nkectl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/cwe"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

const (
	// BinaryEnv overrides which nkectl is driven. Set by CI, where the binary is
	// staged into the runner rather than installed.
	BinaryEnv = "NKECTL_BIN"

	// siteName is the site this package registers. Its own name, in its own
	// config home, so nothing here can disturb a developer's real sites.
	siteName = "e2e-nkectl"

	// forwardReadyTimeout bounds how long kubectl may take to print the line
	// naming the local port it bound.
	forwardReadyTimeout = 30 * time.Second

	// commandTimeout bounds a single nkectl invocation. Every call is bounded
	// because a hung CLI would otherwise stall the spec until Ginkgo's node
	// timeout, long after the useful diagnostics have scrolled past.
	commandTimeout = 2 * time.Minute
)

// forwardingRe matches the line kubectl prints once the local listener is up:
//
//	Forwarding from 127.0.0.1:54321 -> 8080
var forwardingRe = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// Options configures a Runner.
type Options struct {
	// Binary is the nkectl to drive. Empty resolves $NKECTL_BIN, then PATH.
	Binary string

	// Kubeconfig is the management cluster kubeconfig. nkectl mints its API
	// token from it and reads tenant kubeconfig Secrets through it. Empty
	// resolves $KUBECONFIG, then ~/.kube/config.
	Kubeconfig string

	// Context is the kubeconfig context the port-forward uses. Empty uses the
	// current one.
	Context string

	// Timeouts bound the CLI-driven waits. The zero value gets the same
	// defaults the REST client applies.
	Timeouts nke.Options

	// Logf receives progress lines. Optional.
	Logf func(format string, args ...any)
}

// Runner drives nkectl against a site it registers itself, over a port-forward
// it owns. Close releases both.
type Runner struct {
	binary     string
	kubeconfig string
	configHome string
	baseURL    string
	timeouts   nke.Options
	logf       func(format string, args ...any)

	forward       *exec.Cmd
	forwardCancel context.CancelFunc
}

// New resolves the binary, starts a port-forward to the NKE Service and
// registers a site pointed at it. The returned Runner must be closed.
//
// ctx bounds only the setup; the port-forward outlives it and is torn down by
// Close.
func New(ctx context.Context, opts Options) (*Runner, error) {
	binary, err := resolveBinary(opts.Binary)
	if err != nil {
		return nil, err
	}
	kubeconfig, err := resolveKubeconfig(opts.Kubeconfig)
	if err != nil {
		return nil, err
	}

	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	configHome, err := os.MkdirTemp("", "nkectl-e2e-config-")
	if err != nil {
		return nil, fmt.Errorf("creating nkectl config home: %w", err)
	}

	r := &Runner{
		binary:     binary,
		kubeconfig: kubeconfig,
		configHome: configHome,
		timeouts:   withDefaults(opts.Timeouts),
		logf:       logf,
	}

	if err := r.startForward(ctx, opts.Context); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.setSite(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Close stops the port-forward and removes the throwaway config. Safe on a
// partially built Runner, so New can defer to it on any failure.
func (r *Runner) Close() error {
	if r.forwardCancel != nil {
		r.forwardCancel()
		if r.forward != nil {
			_ = r.forward.Wait()
		}
		r.forwardCancel = nil
	}
	if r.configHome != "" {
		err := os.RemoveAll(r.configHome)
		r.configHome = ""
		return err
	}
	return nil
}

// BaseURL is the forwarded NKE endpoint the registered site points at.
func (r *Runner) BaseURL() string { return r.baseURL }

// Run invokes nkectl with args and returns its combined output. The output is
// returned on failure too: nkectl reports the reason there, and an error naming
// only the exit status is not worth reading.
func (r *Runner) Run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	full := append([]string{"--site", siteName}, args...)
	// #nosec G204 -- driving a named binary with arguments this package builds
	// is the entire purpose of the type. Both come from the suite, not input.
	cmd := exec.CommandContext(ctx, r.binary, full...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+r.configHome)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("nkectl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// startForward runs `kubectl port-forward` on an ephemeral local port and waits
// for the line naming it. The port is left to kubectl rather than picked here,
// so nothing can claim it between a probe and the bind.
//
// The forward deliberately outlives ctx, which bounds only New's setup, so its
// own context keeps ctx's values but drops its cancellation. Close is what
// stops it.
func (r *Runner) startForward(ctx context.Context, kubeContext string) error {
	fwdCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.forwardCancel = cancel

	args := []string{
		"--kubeconfig", r.kubeconfig,
		"-n", cwe.Namespace(),
		"port-forward", "svc/" + cwe.Service(),
		"--address", "127.0.0.1",
		":" + cwe.Port(),
	}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}

	cmd := exec.CommandContext(fwdCtx, "kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("piping kubectl port-forward stdout: %w", err)
	}
	cmd.Stderr = os.Stderr

	if startErr := cmd.Start(); startErr != nil {
		cancel()
		return fmt.Errorf("starting kubectl port-forward: %w", startErr)
	}
	r.forward = cmd

	port, err := awaitForwardPort(stdout)
	if err != nil {
		return err
	}
	r.baseURL = fmt.Sprintf("http://127.0.0.1:%s", port)
	r.logf("nkectl e2e: port-forward to %s/%s ready at %s", cwe.Namespace(), cwe.Service(), r.baseURL)
	return nil
}

// awaitForwardPort reads kubectl's stdout until it announces the local port,
// then drains the rest in the background so the pipe never fills and blocks the
// forward.
func awaitForwardPort(stdout io.ReadCloser) (string, error) {
	type result struct {
		port string
		err  error
	}
	found := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		reported := false
		for scanner.Scan() {
			if reported {
				continue // drain
			}
			if m := forwardingRe.FindStringSubmatch(scanner.Text()); m != nil {
				found <- result{port: m[1]}
				reported = true
			}
		}
		if !reported {
			found <- result{err: fmt.Errorf("kubectl port-forward exited before reporting a local port")}
		}
	}()

	select {
	case res := <-found:
		return res.port, res.err
	case <-time.After(forwardReadyTimeout):
		return "", fmt.Errorf("kubectl port-forward did not report a local port within %v", forwardReadyTimeout)
	}
}

// setSite registers the forwarded endpoint through the CLI rather than by
// writing config.yaml directly, so `config set-site` is itself covered.
func (r *Runner) setSite(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(r.configHome, "nkectl"), 0o755); err != nil {
		return fmt.Errorf("creating nkectl config dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	// #nosec G204 -- see Run: the binary and every argument originate here.
	cmd := exec.CommandContext(ctx, r.binary, "config", "set-site",
		"--name", siteName,
		"--url", r.baseURL,
		"--kubeconfig", r.kubeconfig,
	)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+r.configHome)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nkectl config set-site: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveBinary prefers an explicit path, then $NKECTL_BIN, then PATH. The
// error names all three, because "not found" on its own sends the reader
// looking in the wrong place.
func resolveBinary(explicit string) (string, error) {
	for _, candidate := range []string{explicit, os.Getenv(BinaryEnv)} {
		if candidate == "" {
			continue
		}
		// #nosec G703 -- candidate is an operator-supplied path to the binary
		// under test; resolving it is the point, and it is only stat'd here.
		if _, err := os.Stat(candidate); err != nil {
			return "", fmt.Errorf("nkectl binary %q: %w", candidate, err)
		}
		return candidate, nil
	}
	path, err := exec.LookPath("nkectl")
	if err != nil {
		return "", fmt.Errorf("no nkectl binary: pass Options.Binary, set %s, or put nkectl on PATH: %w", BinaryEnv, err)
	}
	return path, nil
}

// resolveKubeconfig mirrors clientcmd's precedence far enough for the CLI,
// which needs a concrete path where the suite is content with default loading.
func resolveKubeconfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		// KUBECONFIG may list several files; nkectl takes one, so the first wins.
		first := strings.Split(env, string(os.PathListSeparator))[0]
		if first != "" {
			return first, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory for the default kubeconfig: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// withDefaults fills the timeouts a caller left zero. Kept in step with the
// REST client's own defaults so the two paths wait the same.
func withDefaults(o nke.Options) nke.Options {
	if o.Provision == 0 {
		o.Provision = 30 * time.Minute
	}
	if o.Delete == 0 {
		o.Delete = 15 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 10 * time.Second
	}
	return o
}
