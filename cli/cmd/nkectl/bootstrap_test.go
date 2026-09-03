// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/bootstrapengine"
	"github.com/NVIDIA/nke/cli/internal/inventory"
	api "github.com/NVIDIA/nke/controller/api"
)

// fakeCWERunner is a scripted cwedeploy.CommandRunner: every helm call
// succeeds, and every `kubectl get --raw .../readyz` call reports healthy
// immediately, so runBootstrapSite's CWE deploy step can be exercised
// without a real helm binary, kubectl binary or cluster.
type fakeCWERunner struct{}

func (fakeCWERunner) Run(_ context.Context, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if name == "kubectl" && strings.Contains(joined, "jsonpath={.data.value}") {
		// The Secret CWE publishes the finished cluster's kubeconfig in.
		return base64.StdEncoding.EncodeToString([]byte("apiVersion: v1\nkind: Config\n")), nil
	}
	if name == "kubectl" && strings.Contains(joined, "kustomize") {
		// The provisioned provider's rendered manifest, carrying the
		// placeholder image cwedeploy rewrites.
		return "        image: docker.io/library/cluster-api-provider-provisioned:latest\n", nil
	}
	if name == "kubectl" && strings.Contains(joined, "readyz") {
		return `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`, nil
	}
	if name == "kubectl" && strings.Contains(joined, "capi-controller-manager") {
		// CAPI is not installed yet, so cwedeploy runs clusterctl.
		return "Error from server (NotFound)", errors.New("exit status 1")
	}
	return "", nil
}

// startFakeCWEAPI stands up an HTTP server that answers the two calls
// `bootstrap site` makes after the deploy — create the cluster, then poll it —
// and points nkectl at it through the port it dials.
//
// A real server rather than an injected client: the address is the thing under
// test on this path. nkectl builds it from the host port the CWE NodePort was
// mapped to, and a client handed in by a test would prove nothing about that.
func startFakeCWEAPI(t *testing.T, provisioned bool) {
	t.Helper()

	id := uuid.New()
	status := api.ConditionStatusTrue
	if !provisioned {
		status = api.ConditionStatusFalse
	}
	cluster := api.Cluster{
		ID:   id,
		Name: "nke-site",
		Conditions: api.ClusterConditions{
			Provisioned:  api.Condition{Status: status},
			Provisioning: api.Condition{Status: api.ConditionStatusTrue},
			Deleting:     api.Condition{Status: api.ConditionStatusFalse},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/clusters", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cluster)
	})
	mux.HandleFunc("GET /api/clusters/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cluster)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake CWE API: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parsing the test server address %q: %v", srv.URL, err)
	}
	t.Setenv("NKECTL_CWE_PORT", port)
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
  nodes:
    - name: node1
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
	if err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{inventory: path, dryRun: true}); err != nil {
		t.Fatalf("dry-run of valid inventory returned error: %v", err)
	}
}

// TestRunBootstrapSiteInvalidFailsFast confirms an invalid inventory errors
// before reaching the (stubbed) dispatch step.
func TestRunBootstrapSiteInvalidFailsFast(t *testing.T) {
	const bad = `
clusterName: nke-site
cluster:
  nodes:
    - name: node1
      ip: not-an-ip
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`
	err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{inventory: writeInventory(t, bad)})
	if err == nil {
		t.Fatal("invalid inventory did not error")
	}
	if strings.Contains(err.Error(), "creating cluster") {
		t.Errorf("reached the create call before validation failed: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster.nodes[0].ip") {
		t.Errorf("error does not name the bad field: %v", err)
	}
}

// TestRunBootstrapSiteCreatesTheCluster is the whole command on its happy
// path: a valid inventory passes validation, the ephemeral bootstrap kind
// cluster is stood up and torn down again, CWE is asked to create the cluster,
// and the finished cluster's kubeconfig is written out.
//
// Every binary is faked, so this needs no kind, no container runtime and no
// cluster; the CWE API is a real HTTP server on the port nkectl dials.
func TestRunBootstrapSiteCreatesTheCluster(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nke-site.kubeconfig")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{
		inventory:       writeInventory(t, validInventory),
		bootstrapRunner: runner,
		cweRunner:       fakeCWERunner{},
		kubeconfigOut:   kubeconfigOut,
	})
	if err != nil {
		t.Fatalf("runBootstrapSite returned error: %v", err)
	}

	if !runner.sawArgs("create", "cluster") {
		t.Errorf("runBootstrapSite did not create the bootstrap kind cluster; calls: %v", runner.calls)
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("runBootstrapSite did not tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
	body, readErr := os.ReadFile(kubeconfigOut)
	if readErr != nil {
		t.Fatalf("no kubeconfig written for the finished cluster: %v", readErr)
	}
	if !strings.Contains(string(body), "kind: Config") {
		t.Errorf("the written kubeconfig is not one: %q", body)
	}
}

// TestRunBootstrapSiteFailsWhenTheClusterNeverComesUp: the command's promise is
// a working cluster, so a cluster that never reports provisioned has to be a
// failure rather than a command that returns 0 and leaves the operator to find
// out later.
func TestRunBootstrapSiteFailsWhenTheClusterNeverComesUp(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, false)
	t.Setenv("NKECTL_BOOTSTRAP_CLUSTER_TIMEOUT", "5ms")

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{
		inventory:       writeInventory(t, validInventory),
		bootstrapRunner: runner,
		cweRunner:       fakeCWERunner{},
		clusterTimeout:  5 * time.Millisecond,
		kubeconfigOut:   filepath.Join(t.TempDir(), "unused.kubeconfig"),
	})
	if err == nil {
		t.Fatal("expected an error when the cluster never reports provisioned")
	}
	if !strings.Contains(err.Error(), "did not report provisioned") {
		t.Errorf("error %q does not say the cluster never came up", err)
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after site cluster creation started")
	}
	if !strings.Contains(err.Error(), "retained for recovery") || !strings.Contains(err.Error(), "kubeconfig:") {
		t.Errorf("error does not explain how to recover from the retained cluster: %v", err)
	}
}

func TestRunBootstrapSiteRetainsClusterWhenKubeconfigIsNotPublished(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{
		inventory:         writeInventory(t, validInventory),
		bootstrapRunner:   runner,
		cweRunner:         missingKubeconfigRunner{},
		kubeconfigTimeout: 5 * time.Millisecond,
		kubeconfigOut:     filepath.Join(t.TempDir(), "unused.kubeconfig"),
	})
	if err == nil {
		t.Fatal("expected an error when the kubeconfig Secret is not published")
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after the site cluster became provisioned")
	}
	for _, want := range []string{"not published", "retained for recovery", "kind delete cluster"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// setBootstrapEnv fills in the images `bootstrap site` requires, so a test
// about something else does not fail on one of them.
func setBootstrapEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NKECTL_CWE_IMAGE", "nvcr.io/org/controller:edge")
	t.Setenv("NKECTL_CWE_PROFILES_IMAGE", "nvcr.io/org/cwe-profiles:v1")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "cluster-api-provider-provisioned:local")
	t.Setenv("NKECTL_CAPI_PROVISIONED_PATH", t.TempDir())
	t.Setenv("NKECTL_CWE_CHART_PATH", t.TempDir())
}

// The image lookup must run against the runtime kind loads from. nerdctl is
// the default provider, and such a host need not have Docker at all.
func TestRunBootstrapSiteUsesTheProviderCLI(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	// --bootstrap-provider value -> the CLI the lookup must use.
	for provider, wantCLI := range map[string]string{"": "nerdctl", "docker": "docker"} {
		runner := &recordingRunner{next: fakeBootstrapRunner{}}
		_ = runBootstrapSite(io.Discard, &bootstrapSiteOptions{
			inventory:       writeInventory(t, validInventory),
			provider:        provider,
			bootstrapRunner: runner,
			cweRunner:       fakeCWERunner{},
			kubeconfigOut:   filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
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
	setBootstrapEnv(t)
	t.Setenv("NKECTL_CWE_IMAGE", "")

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{
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

	var buf bytes.Buffer
	if err := dumpInventory(&buf, inv); err != nil {
		t.Fatalf("dumpInventory: %v", err)
	}
	out := buf.String()

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

// TestRunBootstrapSite_DiagnosticsOnlyOnRealFailures guards the distinction the
// dump depends on. A pre-create failure still tears the bootstrap cluster down,
// so its bundle has to be collected first. A successful run writes no bundle.
func TestRunBootstrapSite_DiagnosticsOnlyOnRealFailures(t *testing.T) {
	setBootstrapEnv(t)

	t.Run("a successful bootstrap writes nothing", func(t *testing.T) {
		startFakeCWEAPI(t, true)
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{
			inventory:       writeInventory(t, validInventory),
			diagnosticsDir:  dir,
			bootstrapRunner: fakeBootstrapRunner{},
			cweRunner:       fakeCWERunner{},
			kubeconfigOut:   filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
		})
		if err != nil {
			t.Fatalf("runBootstrapSite returned error: %v", err)
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
		err := runBootstrapSite(io.Discard, &bootstrapSiteOptions{
			inventory:       writeInventory(t, validInventory),
			diagnosticsDir:  dir,
			bootstrapRunner: fakeBootstrapRunner{},
			cweRunner:       unhealthyCWERunner{},
		})
		if err == nil {
			t.Fatal("expected a real failure")
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

type missingKubeconfigRunner struct{ fakeCWERunner }

func (r missingKubeconfigRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "kubectl" && strings.Contains(strings.Join(args, " "), "jsonpath={.data.value}") {
		return "Error from server (NotFound): secret not found", errors.New("exit status 1")
	}
	return r.fakeCWERunner.Run(ctx, name, args...)
}
