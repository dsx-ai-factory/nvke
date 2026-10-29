// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e_bootstrap

package bootstraptest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dsx-ai-factory/nvke/cli/e2e/bootstrap/bootstraptest"
)

// siteStackCheckTimeout bounds the API calls of the one-shot site stack check.
const siteStackCheckTimeout = time.Minute

// credentialCheckTimeout bounds the reads of the credential check that follows
// the bootstrap.
const credentialCheckTimeout = 2 * time.Minute

// errStopped is how the bootstrap ended when the lane signalled this process.
var errStopped = errors.New("stopped on a signal")

// TestE2E_BootstrapSite runs one `nvkectl bootstrap site` and reports what it
// exited with. It stands in for the command itself, in the place the bootstrap
// lanes used to invoke it: run.sh and run-nico.sh start this in the
// background, watch the log file it writes, bridge their networks, and then
// wait for it, exactly as they did around `setsid nvkectl bootstrap site`.
//
// The lane configures the run through the environment; see env.go. It is not a
// lane of its own. With EnvSiteStackNamespaces set it also checks the NVKE
// stack on the site cluster. With EnvKnownSecretsFile set it gives nvkectl an
// OTLP header credential and, once the command exits, searches its log and
// diagnostics for that and the secrets bootstrap generated. Everything else
// asserted about the run is still asserted by the lane, over
// NVKECTL_E2E_BOOTSTRAP_LOG.
//
// The TestE2E_ prefix is how the lanes select it; see the package comment.
//
// -timeout 0 matters when invoking it: a bootstrap outlasts the 10 minute
// default, and a go test timeout panics the process rather than stopping the
// command.
func TestE2E_BootstrapSite(t *testing.T) {
	// The test's working directory is its own package directory, so cli/ is
	// three levels up. NVKECTL overrides the binary this resolves anyway.
	opts, err := bootstraptest.BootstrapOptionsFromEnv("../../..")
	if err != nil {
		t.Fatalf("reading the run configuration: %v", err)
	}

	check, err := startCredentialCheck(t, &opts)
	if err != nil {
		t.Fatalf("starting the credential check: %v", err)
	}

	b, err := bootstraptest.StartBootstrap(context.Background(), opts, t.Logf)
	if err != nil {
		t.Fatalf("starting bootstrap site: %v", err)
	}
	t.Cleanup(b.Stop)
	stopOnSignal(t, b, check)

	waitErr := b.Wait()
	// Before the lane reads the log: a log that holds a secret is replaced.
	if err := check.finish(waitErr); err != nil {
		t.Errorf("the credential check failed: %v", err)
	}
	if waitErr != nil {
		t.Fatalf("nvkectl bootstrap site exited %d: %v", b.ExitCode(), waitErr)
	}
	t.Logf("bootstraptest: nvkectl bootstrap site exited 0")

	if namespaces := bootstraptest.SiteStackNamespacesFromEnv(opts.DisableMTLS); len(namespaces) > 0 {
		verifySiteStack(t, opts.KubeconfigOut, namespaces)
	}
}

// verifySiteStack checks the NVKE stack the bootstrap installed and reported
// healthy on the site cluster that kubeconfig reaches. It does not wait for it.
func verifySiteStack(t *testing.T, kubeconfig string, namespaces []string) {
	t.Helper()
	if kubeconfig == "" {
		t.Fatalf("%s is set but %s is not: the site cluster cannot be reached", bootstraptest.EnvSiteStackNamespaces, bootstraptest.EnvKubeconfigOut)
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("loading the site kubeconfig %s: %v", kubeconfig, err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("creating the site cluster client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), siteStackCheckTimeout)
	defer cancel()
	if err := bootstraptest.VerifySiteStack(ctx, kube, namespaces); err != nil {
		t.Fatal(err)
	}
	t.Logf("bootstraptest: the NVKE stack on the site cluster is rolled out and Ready: %s", strings.Join(namespaces, " "))
}

// credentialCheck is the credential check the lane asked for, run at most once
// whichever of the end of the bootstrap or a signal comes first. It does
// nothing when the lane did not ask for one.
type credentialCheck struct {
	scan *bootstraptest.SecretScan
	once sync.Once
	err  error
}

// startCredentialCheck reads the lane's request from the environment and, when
// there is one, adds the OTLP settings to opts.Env and starts the check.
func startCredentialCheck(t *testing.T, opts *bootstraptest.BootstrapOptions) (*credentialCheck, error) {
	t.Helper()
	scan, err := bootstraptest.NewSecretScanFromEnv(*opts)
	if err != nil || scan == nil {
		return &credentialCheck{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := scan.Begin(ctx); err != nil {
		return nil, err
	}
	opts.Env = append(opts.Env, scan.Env()...)
	return &credentialCheck{scan: scan}, nil
}

// finish runs the check once and returns its result on every call.
func (c *credentialCheck) finish(bootstrapErr error) error {
	if c.scan == nil {
		return nil
	}
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), credentialCheckTimeout)
		defer cancel()
		c.err = c.scan.Finish(ctx, bootstrapErr)
	})
	return c.err
}

// TestScanBootstrapOutputs searches the bootstrap log, the Go test log and the
// diagnostics for the secret values TestE2E_BootstrapSite saved, and fails on a
// hit or an incomplete search. The lane runs it at its end.
//
// It has no TestE2E_ prefix: the lanes select that with the bootstrap, and this
// has to run after it.
func TestScanBootstrapOutputs(t *testing.T) {
	if err := bootstraptest.ScanKnownSecretsFromEnv(); err != nil {
		t.Fatal(err)
	}
	t.Logf("bootstraptest: no known secret value in the bootstrap output or its diagnostics")
}

// stopOnSignal ends the bootstrap when this process is signalled.
//
// The lane aborts by signalling this process's group, and nvkectl is not in
// it: it is given a group of its own so that the kind, helm and nerdctl it
// drives can be signalled as one. Without this, an aborted lane would leave
// nvkectl running and then tear down the cluster underneath it -- which is
// what the shell's `kill -- -$bootstrap_pid` existed to prevent.
func stopOnSignal(t *testing.T, b *bootstraptest.Bootstrap, check *credentialCheck) {
	t.Helper()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(signals) })

	go func() {
		sig, ok := <-signals
		if !ok {
			return
		}
		// Not t.Fatalf: this is not the test's goroutine, and the lane is
		// already failing. Stop the command, then go, so the lane's teardown
		// does not race a bootstrap that is still driving the cluster.
		b.Stop()
		// The lane prints the log next, so it is checked first.
		if err := check.finish(errStopped); err != nil {
			fmt.Fprintf(os.Stderr, "bootstraptest: the credential check failed: %v\n", err)
		}
		os.Stderr.WriteString("bootstraptest: stopped bootstrap site on " + sig.String() + "\n")
		os.Exit(1)
	}()
}
