// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/nke/cli/internal/bootstrapengine"
	"github.com/NVIDIA/nke/cli/internal/inventory"
)

// fakeCWERunner is a scripted cwedeploy.CommandRunner: every helm call
// succeeds, and every `kubectl get --raw .../readyz` call reports healthy
// immediately, so runBootstrapSite's CWE deploy step can be exercised
// without a real helm binary, kubectl binary or cluster.
type fakeCWERunner struct{}

func (fakeCWERunner) Run(_ context.Context, name string, _ ...string) (string, error) {
	if name == "kubectl" {
		return `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`, nil
	}
	return "", nil
}

// fakeBootstrapRunner is a scripted bootstrapengine.CommandRunner so
// runBootstrapSite can be exercised without a real kind binary or container
// runtime. It answers every invocation the happy path issues: no existing
// cluster, a successful create, and a resolvable host manifest digest.
type fakeBootstrapRunner struct{}

func (fakeBootstrapRunner) Run(_ context.Context, _ map[string]string, name string, args ...string) (string, error) {
	if len(args) < 2 {
		return "", nil
	}
	if name == "kind" && args[0] == "get" && args[1] == "clusters" {
		// Mirrors kind's own exit-1-with-message behavior when no
		// clusters exist, which bootstrapengine.Exists treats as
		// "false, nil" rather than a real error.
		return "No kind clusters found.\n", errors.New("exit status 1")
	}
	if args[0] == "image" && args[1] == "inspect" {
		// Target digest and host manifest digest, equal: nothing to flatten.
		d := "sha256:" + strings.Repeat("a", 64)
		return d + " " + d + "\n", nil
	}
	return "", nil
}

const validInventory = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`

// TestBootstrapSiteCommandWired verifies the command tree exposes
// "nkectl bootstrap site" — the runnable command required by the ticket.
func TestBootstrapSiteCommandWired(t *testing.T) {
	root := newRootCmd("test", "test", "test", "test")
	cmd, _, err := root.Find([]string{"bootstrap", siteSubcommand})
	if err != nil {
		t.Fatalf("find bootstrap site: %v", err)
	}
	if cmd.Name() != siteSubcommand {
		t.Fatalf("resolved command = %q, want %q", cmd.Name(), siteSubcommand)
	}
}

// TestRunBootstrapSiteDryRunValid confirms a valid inventory passes validation
// and dry-run returns no error without dispatching anything.
func TestRunBootstrapSiteDryRunValid(t *testing.T) {
	path := writeInventory(t, validInventory)
	if err := runBootstrapSite(&bootstrapSiteOptions{inventory: path, dryRun: true}); err != nil {
		t.Fatalf("dry-run of valid inventory returned error: %v", err)
	}
}

// TestRunBootstrapSiteInvalidFailsFast confirms an invalid inventory errors
// before reaching the (stubbed) dispatch step.
func TestRunBootstrapSiteInvalidFailsFast(t *testing.T) {
	const bad = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: not-an-ip
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`
	err := runBootstrapSite(&bootstrapSiteOptions{inventory: writeInventory(t, bad)})
	if err == nil {
		t.Fatal("invalid inventory did not error")
	}
	if strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("reached dispatch stub before validation failed: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster.initNode.ip") {
		t.Errorf("error does not name the bad field: %v", err)
	}
}

// TestRunBootstrapSiteValidReachesStub confirms a valid inventory (no
// dry-run) passes validation, stands up and tears down the ephemeral
// bootstrap kind cluster, and reaches the not-yet-implemented dispatch stub.
// The kind/kubectl calls go through fakeBootstrapRunner, so this needs no
// real kind binary or container runtime.
func TestRunBootstrapSiteValidReachesStub(t *testing.T) {
	t.Setenv("NKECTL_CWE_IMAGE", "nvcr.io/org/controller:edge")
	t.Setenv("NKECTL_CWE_PROFILES_IMAGE", "nvcr.io/org/cwe-profiles:v1")

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(&bootstrapSiteOptions{
		inventory:       writeInventory(t, validInventory),
		bootstrapRunner: runner,
		cweRunner:       fakeCWERunner{},
	})
	if err == nil {
		t.Fatal("expected the dispatch stub error")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("expected dispatch stub error, got: %v", err)
	}

	if !runner.sawArgs("create", "cluster") {
		t.Errorf("runBootstrapSite did not create the bootstrap kind cluster; calls: %v", runner.calls)
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("runBootstrapSite did not tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
}

// The image lookup must run against the runtime kind loads from. nerdctl is
// the default provider, and such a host need not have Docker at all.
func TestRunBootstrapSiteUsesTheProviderCLI(t *testing.T) {
	t.Setenv("NKECTL_CWE_IMAGE", "nvcr.io/org/controller:edge")
	t.Setenv("NKECTL_CWE_PROFILES_IMAGE", "nvcr.io/org/cwe-profiles:v1")

	// --bootstrap-provider value -> the CLI the lookup must use.
	for provider, wantCLI := range map[string]string{"": "nerdctl", "docker": "docker"} {
		runner := &recordingRunner{next: fakeBootstrapRunner{}}
		_ = runBootstrapSite(&bootstrapSiteOptions{
			inventory:       writeInventory(t, validInventory),
			provider:        provider,
			bootstrapRunner: runner,
			cweRunner:       fakeCWERunner{},
		})
		if !slices.Contains(runner.names, wantCLI) {
			t.Errorf("provider %q: no %s call; ran %v", provider, wantCLI, runner.names)
		}
	}
}

// TestRunBootstrapSiteMissingCWEImage confirms the command fails with a
// clear, actionable error when NKECTL_CWE_IMAGE is not set, rather than
// reaching helm with an empty image reference.
//
// It must also fail before the Kind cluster is built. What to deploy is
// settled up front, so a setting the operator never provided costs them a
// message rather than a cluster that is stood up only to be torn down.
func TestRunBootstrapSiteMissingCWEImage(t *testing.T) {
	t.Setenv("NKECTL_CWE_IMAGE", "")

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(&bootstrapSiteOptions{
		inventory:       writeInventory(t, validInventory),
		bootstrapRunner: runner,
		cweRunner:       fakeCWERunner{},
	})
	if err == nil {
		t.Fatal("expected an error for a missing NKECTL_CWE_IMAGE")
	}
	if !strings.Contains(err.Error(), "NKECTL_CWE_IMAGE") {
		t.Errorf("error does not name the missing setting: %v", err)
	}

	if runner.sawArgs("create", "cluster") {
		t.Errorf("a missing NKECTL_CWE_IMAGE should be caught before the cluster is built; calls: %v", runner.calls)
	}
}

// recordingRunner wraps another CommandRunner and records the command name
// and args of every call, so a test can assert the bootstrap cluster was
// actually created and torn down without depending on bootstrapengine's own
// internals.
type recordingRunner struct {
	next  bootstrapengine.CommandRunner
	names []string
	calls [][]string
}

func (r *recordingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	r.names = append(r.names, name)
	r.calls = append(r.calls, args)
	return r.next.Run(ctx, env, name, args...)
}

func (r *recordingRunner) sawArgs(prefix ...string) bool {
	for _, call := range r.calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func writeInventory(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	return path
}

// TestDumpInventoryRedactsRegistryAPIKey covers the one branch in this package
// that exists purely to keep a credential out of the terminal. Nothing else
// asserted it: every other inventory in these tests omits apiKey, so the
// redaction line had an execution count of zero and a regression that printed
// the key verbatim would have passed.
func TestDumpInventoryRedactsRegistryAPIKey(t *testing.T) {
	const secret = "nvapi-do-not-print-me"

	inv, err := inventory.Load(writeInventory(t, validInventory+"  apiKey: "+secret+"\n"))
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if inv.Registry.APIKey != secret {
		t.Fatalf("fixture did not set the api key: got %q, want %q", inv.Registry.APIKey, secret)
	}

	out := captureStdout(t, func() {
		if err := dumpInventory(inv); err != nil {
			t.Fatalf("dumpInventory: %v", err)
		}
	})

	if strings.Contains(out, secret) {
		t.Errorf("dumpInventory printed the registry credential:\n%s", out)
	}
	if !strings.Contains(out, "***redacted***") {
		t.Errorf("dumpInventory did not mark the credential as redacted:\n%s", out)
	}
	// Redaction works on a copy, so the caller's inventory must be left intact:
	// whatever runs next still needs the real credential.
	if inv.Registry.APIKey != secret {
		t.Errorf("dumpInventory mutated the caller's inventory: api key is now %q", inv.Registry.APIKey)
	}
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what was
// written. dumpInventory writes to os.Stdout directly rather than to an
// injected writer, so this is the only way to observe it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	return <-done
}

// TestRunBootstrapSite_DiagnosticsOnlyOnRealFailures guards the distinction the
// dump depends on. Every run ends in an error today — the dispatch stub — so
// dumping on "returned an error" would collect a cluster bundle on every
// successful bootstrap, and bury the real ones.
func TestRunBootstrapSite_DiagnosticsOnlyOnRealFailures(t *testing.T) {
	t.Setenv("NKECTL_CWE_IMAGE", "nvcr.io/org/controller:edge")
	t.Setenv("NKECTL_CWE_PROFILES_IMAGE", "nvcr.io/org/cwe-profiles:v1")
	t.Setenv("NKECTL_CWE_CHART_PATH", t.TempDir())

	t.Run("the dispatch stub writes nothing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		err := runBootstrapSite(&bootstrapSiteOptions{
			inventory:       writeInventory(t, validInventory),
			diagnosticsDir:  dir,
			bootstrapRunner: fakeBootstrapRunner{},
			cweRunner:       fakeCWERunner{},
		})
		if !errors.Is(err, errDispatchNotImplemented) {
			t.Fatalf("expected the dispatch stub, got %v", err)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Error("a successful bootstrap should leave no diagnostics behind")
		}
	})

	t.Run("a real failure writes a bundle", func(t *testing.T) {
		// Short, so the suite does not sit through the default five minutes
		// waiting for a CWE that will never report healthy.
		t.Setenv("NKECTL_CWE_READY_TIMEOUT", "1s")
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		// CWE never reports healthy: the failure this exists for.
		err := runBootstrapSite(&bootstrapSiteOptions{
			inventory:       writeInventory(t, validInventory),
			diagnosticsDir:  dir,
			bootstrapRunner: fakeBootstrapRunner{},
			cweRunner:       unhealthyCWERunner{},
		})
		if err == nil || errors.Is(err, errDispatchNotImplemented) {
			t.Fatalf("expected a real failure, got %v", err)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Errorf("no diagnostics written for a real failure: %v", statErr)
		}
	})
}

// unhealthyCWERunner deploys fine but never reports healthy, which is the
// shape of the failure the diagnostics bundle is meant to explain.
type unhealthyCWERunner struct{ fakeCWERunner }

func (u unhealthyCWERunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "kubectl" && strings.Contains(strings.Join(args, " "), "readyz") {
		return `{"status":"starting"}`, nil
	}
	return u.fakeCWERunner.Run(ctx, name, args...)
}
