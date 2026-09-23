// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nkectl drives the nkectl binary against the NKE deployment the e2e
// suite already has running. It is the command-line counterpart to
// internal/nke, the REST client the rest of the suite uses; both build their
// create request through nke.BuildCreateRequest, so the two differ only in
// transport.
//
// A Runner owns a `kubectl port-forward` to the NKE Service and a throwaway
// site config pointed at it. It cannot share the suite's forward:
// kube.ServiceForwarder routes through a custom DialContext against a synthetic
// .svc authority that never resolves, so no separate process can reach it.
package nkectl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/cwe"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
)

const (
	// BinaryEnv overrides which nkectl is driven. Set by CI, where the binary is
	// staged into the runner rather than installed.
	BinaryEnv = "NKECTL_BIN"

	// kubectlBinary carries the port-forward, and is the only command besides
	// nkectl that a Runner needs.
	kubectlBinary = "kubectl"

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

var (
	// forwardingRe matches the line kubectl prints once the local listener is up:
	//
	//	Forwarding from 127.0.0.1:54321 -> 8080
	forwardingRe = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

	// ErrBinaryNotFound reports that a binary a Runner needs -- nkectl, or the
	// kubectl carrying its port-forward -- is not installed. New wraps it, so a
	// caller that tolerates an environment without them should test for it with
	// errors.Is and treat every other error from New as a failure.
	ErrBinaryNotFound = errors.New("required binary not found")
)

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
// it owns. Close releases both. Not safe for concurrent use.
type Runner struct {
	binary     string
	kubeconfig string
	configHome string
	baseURL    string
	timeouts   nke.Options
	logf       func(format string, args ...any)

	forward       *exec.Cmd
	forwardCancel context.CancelFunc
	// forwardDone closes when kubectl's stdout reaches EOF. `kubectl
	// port-forward` exits when its pod goes away rather than re-resolving, so
	// this is what makes a dead forward visible to ensureForward.
	forwardDone chan struct{}

	// clusterNames maps id to name. nkectl addresses clusters by name and the
	// suite's seam passes ids, so the two have to be reconciled somewhere.
	clusterNames map[openapi_types.UUID]string
	// createdClusters names every cluster CreateCluster may have brought into
	// being, including one whose create response was lost. Names, because that
	// is all CleanupCreated needs and all that is known before the reply.
	createdClusters []string
	// createdNodePools names every node pool CreateNodePool may have brought
	// into being, keyed the way nkectl addresses one.
	createdNodePools []NodePoolRef
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
	// Checked up front so a host without kubectl reports ErrBinaryNotFound like
	// one without nkectl. Neither is a regression in the CLI under test.
	if _, lookErr := exec.LookPath(kubectlBinary); lookErr != nil {
		return nil, fmt.Errorf("%w: %s carries the port-forward: %w", ErrBinaryNotFound, kubectlBinary, lookErr)
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
		binary:       binary,
		kubeconfig:   kubeconfig,
		configHome:   configHome,
		timeouts:     nke.WithDefaults(opts.Timeouts),
		logf:         logf,
		clusterNames: map[openapi_types.UUID]string{},
	}

	// A requested context has to be baked into the file, not passed per
	// command: nkectl takes a kubeconfig path and no context flag, so it would
	// otherwise mint tokens and read tenant Secrets through the file's
	// current-context while the forward pointed at the requested one.
	if opts.Context != "" {
		flattened, flattenErr := r.flattenToContext(ctx, opts.Context)
		if flattenErr != nil {
			r.Close()
			return nil, flattenErr
		}
		r.kubeconfig = flattened
	}

	if err := r.startForward(ctx); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.setSite(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// flattenToContext writes a kubeconfig holding only the requested context, with
// its credentials embedded, and returns its path. Everything downstream then
// agrees on one cluster without needing to thread a context flag that nkectl
// does not accept.
func (r *Runner) flattenToContext(ctx context.Context, kubeContext string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	// #nosec G204 -- the kubeconfig path and context name come from the
	// suite's own config file, not from input.
	cmd := exec.CommandContext(ctx, kubectlBinary,
		"--kubeconfig", r.kubeconfig,
		"--context", kubeContext,
		"config", "view", "--minify", "--flatten", "--raw", "-o", "yaml",
	)
	// Stderr captured separately: kubectl explains a bad context name there, and
	// Output alone would reduce that to a bare exit status.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("flattening kubeconfig %q to context %q: %w: %s",
			r.kubeconfig, kubeContext, err, strings.TrimSpace(stderr.String()))
	}

	path := filepath.Join(r.configHome, "kubeconfig.yaml")
	// 0600: this holds the embedded client credential --minify --raw emitted.
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", fmt.Errorf("writing the flattened kubeconfig: %w", err)
	}
	r.logf("nkectl e2e: flattened kubeconfig to context %q", kubeContext)
	return path, nil
}

// Close stops the port-forward and removes the throwaway config. It is safe to
// call on a partially initialized Runner.
func (r *Runner) Close() error {
	r.stopForward()
	if r.configHome != "" {
		err := os.RemoveAll(r.configHome)
		r.configHome = ""
		return err
	}
	return nil
}

// stopForward ends the kubectl process and waits for it, leaving the Runner
// ready for startForward to be called again.
func (r *Runner) stopForward() {
	if r.forwardCancel == nil {
		return
	}
	r.forwardCancel()
	if r.forwardDone != nil {
		<-r.forwardDone // the drain goroutine has let go of the pipe
	}
	if r.forward != nil {
		_ = r.forward.Wait()
		r.forward = nil
	}
	r.forwardCancel = nil
	r.forwardDone = nil
	// Cleared so a command issued against a stopped Runner fails on an empty
	// URL rather than quietly reaching for a port that is no longer bound.
	r.baseURL = ""
}

// ensureForward starts a port-forward unless a live one is already running.
//
// It covers two cases: kubectl exiting because the pod behind the Service was
// replaced, and a previous start that failed and so left the Runner with no
// forward at all. Either way the replacement binds a fresh ephemeral port, so
// the site is re-registered to match.
func (r *Runner) ensureForward(ctx context.Context) error {
	if r.forwardDone != nil {
		select {
		case <-r.forwardDone:
			r.logf("nkectl e2e: the port-forward exited; restarting it")
		default:
			return nil // still up
		}
	}

	r.stopForward()
	if err := r.startForward(ctx); err != nil {
		return fmt.Errorf("starting the port-forward: %w", err)
	}
	if err := r.setSite(ctx); err != nil {
		// Drop it: nkectl reads its URL from the site config on disk, so a live
		// forward that config does not name would pass the check above while
		// every command went to the previous URL. The next call starts over.
		r.stopForward()
		return fmt.Errorf("re-registering the site after starting the port-forward: %w", err)
	}
	return nil
}

// BaseURL is the forwarded NKE endpoint the registered site points at.
func (r *Runner) BaseURL() string { return r.baseURL }

// Run invokes nkectl with args and returns its combined output. The output is
// returned on failure too, because nkectl reports the reason there.
func (r *Runner) Run(ctx context.Context, args ...string) (string, error) {
	if err := r.ensureForward(ctx); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	argv := append([]string{"--site", siteName}, args...)
	// #nosec G204 -- driving a named binary with arguments this package builds
	// is the entire purpose of the type. Both come from the suite, not input.
	cmd := exec.CommandContext(ctx, r.binary, argv...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+r.configHome)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("nkectl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// startForward runs `kubectl port-forward` on an ephemeral local port and
// blocks until kubectl names it.
//
// The forward outlives ctx, which bounds only the caller's setup: its own
// context keeps ctx's values but drops its cancellation. stopForward ends it.
func (r *Runner) startForward(ctx context.Context) error {
	fwdCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	// The port is kubectl's to choose, so nothing can claim it between a probe
	// here and the bind there. No --context: r.kubeconfig already names the one
	// to use, flattened by flattenToContext when the caller asked for it.
	args := []string{
		"--kubeconfig", r.kubeconfig,
		"-n", cwe.Namespace(),
		"port-forward", "svc/" + cwe.Service(),
		"--address", "127.0.0.1",
		":" + cwe.Port(),
	}

	// #nosec G204 -- the kubeconfig path is the suite's and the rest is built
	// from this package's own constants.
	cmd := exec.CommandContext(fwdCtx, kubectlBinary, args...)
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

	done := make(chan struct{})
	port, err := awaitForwardPort(stdout, done)
	if err != nil {
		// Kill the process and wait for the drain goroutine to let go of the
		// pipe before reaping it. Nothing is assigned to the Runner on this
		// path: a half-built forward recorded here would read as a live one to
		// ensureForward, which would then send every later command to a port
		// that was never opened.
		cancel()
		<-done
		_ = cmd.Wait()
		return err
	}

	r.forwardCancel = cancel
	r.forward = cmd
	r.forwardDone = done
	r.baseURL = fmt.Sprintf("http://127.0.0.1:%s", port)
	r.logf("nkectl e2e: port-forward to %s/%s ready at %s", cwe.Namespace(), cwe.Service(), r.baseURL)
	return nil
}

// awaitForwardPort reads kubectl's stdout until it announces the local port,
// then drains the rest in the background so the pipe never fills and blocks the
// forward. It closes done when that stdout reaches EOF.
func awaitForwardPort(stdout io.ReadCloser, done chan struct{}) (string, error) {
	type result struct {
		port string
		err  error
	}
	results := make(chan result, 1)

	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		reported := false
		for scanner.Scan() {
			if reported {
				continue // drain
			}
			if match := forwardingRe.FindStringSubmatch(scanner.Text()); match != nil {
				results <- result{port: match[1]}
				reported = true
			}
		}
		if !reported {
			results <- result{err: errors.New("kubectl port-forward exited before reporting a local port")}
		}
	}()

	select {
	case res := <-results:
		return res.port, res.err
	case <-time.After(forwardReadyTimeout):
		return "", fmt.Errorf("kubectl port-forward did not report a local port within %v", forwardReadyTimeout)
	}
}

// setSite registers the forwarded endpoint as the site every Run targets. It
// goes through `config set-site` rather than writing config.yaml directly, so
// that command is covered too.
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

// resolveBinary returns the nkectl to drive, preferring explicit, then
// $NKECTL_BIN, then PATH. A binary found in none of them wraps
// ErrBinaryNotFound.
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
		return "", fmt.Errorf("%w: pass Options.Binary, set %s, or put nkectl on PATH: %w", ErrBinaryNotFound, BinaryEnv, err)
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
