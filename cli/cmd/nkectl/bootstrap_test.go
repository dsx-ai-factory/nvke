// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/controllerdeploy"
	bootstrapengine "github.com/dsx-ai-factory/nke/cli/internal/bootstrap/engine"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/sitespec"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

// fakeCWERunner is a scripted controllerdeploy.CommandRunner: every helm call
// succeeds, and every `kubectl get --raw .../readyz` call reports healthy
// immediately, so runBootstrapSite's controller deploy step can be exercised
// without a real helm binary, kubectl binary or cluster.
type fakeCWERunner struct{}

func (fakeCWERunner) Run(_ context.Context, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if name == "kubectl" && strings.Contains(joined, "jsonpath={.data.value}") {
		// The Secret the controller publishes the finished cluster's kubeconfig in.
		return base64.StdEncoding.EncodeToString([]byte("apiVersion: v1\nkind: Config\n")), nil
	}
	if name == "kubectl" && strings.Contains(joined, "get secret etcd-encryption-key-") {
		// The Secret the controller keeps the cluster's etcd encryption key in.
		return `{"apiVersion":"v1","kind":"Secret","data":{"active":"` +
			base64.StdEncoding.EncodeToString([]byte(fakeEtcdKey)) + `","active-since":"MQ=="}}`, nil
	}
	if name == "kubectl" && strings.Contains(joined, "readyz") {
		return `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`, nil
	}
	if name == "kubectl" && strings.Contains(joined, "capi-controller-manager") {
		// CAPI is not installed yet, so controllerdeploy runs clusterctl.
		return "Error from server (NotFound)", errors.New("exit status 1")
	}
	return "", nil
}

// startFakeCWEAPI stands up an HTTP server that answers the two calls
// `bootstrap site` makes after the deploy — create the cluster, then poll it —
// and points nkectl at it through the port it dials.
//
// A real server rather than an injected client: the address is the thing under
// test on this path. nkectl builds it from the host port the controller NodePort was
// mapped to, and a client handed in by a test would prove nothing about that.
func startFakeCWEAPI(t *testing.T, provisioned bool) {
	t.Helper()
	joined := 0
	if provisioned {
		joined = 1
	}
	startFakeCWEAPIWithNodes(t, provisioned, 1, joined)
}

// startFakeCWEAPIWithNodes is startFakeCWEAPI with the control-plane node list
// spelled out: total nodes, of which the first joined have finished
// provisioning. It is how a test holds a control plane short of the count the
// site spec asked for, which is the state the cluster's own Provisioned
// condition cannot distinguish from a finished one.
func startFakeCWEAPIWithNodes(t *testing.T, provisioned bool, total, joined int) {
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
	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, _ *http.Request) {
		nodes := make([]api.Node, 0, total)
		for i := range total {
			status, message := api.ConditionStatusFalse, "Joining node"
			if i < joined {
				status, message = api.ConditionStatusTrue, "Provisioned node"
			}
			nodes = append(nodes, api.Node{
				Name:         fmt.Sprintf("node%d", i+1),
				ControlPlane: true,
				Conditions: api.NodeConditions{
					Provisioned: api.Condition{Status: status, Message: message},
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ListNodesResponse{Nodes: nodes})
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake controller API: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parsing the test server address %q: %v", srv.URL, err)
	}
	t.Setenv("NKECTL_CONTROLLER_PORT", port)
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

const validSiteSpec = `
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

const threeNodeSiteSpec = `
clusterName: nke-site
cluster:
  nodes:
    - name: node1
      ip: 10.0.0.1
    - name: node2
      ip: 10.0.0.2
    - name: node3
      ip: 10.0.0.3
  controlPlaneVIP: 10.0.0.100
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

// TestBootstrapSiteImageFlags pins the two image flags by name, so the
// registration cannot drift from the help text and the docs that name them.
func TestBootstrapSiteImageFlags(t *testing.T) {
	root := newRootCmd("test", "test", "test", "test")
	cmd, _, err := root.Find([]string{"bootstrap", siteSubcommand})
	if err != nil {
		t.Fatalf("find bootstrap site: %v", err)
	}
	for _, name := range []string{"controller-image", "provisioned-provider-image"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("bootstrap site has no --%s flag", name)
		}
	}
	if cmd.Flags().Lookup("cwe-image") != nil {
		t.Error("bootstrap site still registers --cwe-image")
	}
}

// TestRunBootstrapSiteDryRunValid confirms a valid site spec passes validation
// and dry-run returns no error without dispatching anything.
func TestRunBootstrapSiteDryRunValid(t *testing.T) {
	path := writeSiteSpec(t, validSiteSpec)
	if err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: path, dryRun: true}); err != nil {
		t.Fatalf("dry-run of valid site spec returned error: %v", err)
	}
}

// TestRunBootstrapSiteInvalidFailsFast confirms an invalid site spec errors
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
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: writeSiteSpec(t, bad)})
	if err == nil {
		t.Fatal("invalid site spec did not error")
	}
	if strings.Contains(err.Error(), "creating cluster") {
		t.Errorf("reached the create call before validation failed: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster.nodes[0].ip") {
		t.Errorf("error does not name the bad field: %v", err)
	}
}

// TestRunBootstrapSiteCreatesTheCluster is the whole command on its happy
// path: a valid site spec passes validation, the ephemeral bootstrap kind
// cluster is stood up and torn down again, the controller is asked to create the cluster,
// and the finished cluster's kubeconfig is written out.
//
// Every binary is faked, so this needs no kind, no container runtime and no
// cluster; the controller API is a real HTTP server on the port nkectl dials.
func TestRunBootstrapSiteCreatesTheCluster(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nke-site.kubeconfig")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	spireEntriesDumped := false
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       kubeconfigOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(_ context.Context, source, outputDir string) (int, error) {
			spireEntriesDumped = true
			if source == "" {
				t.Error("source kubeconfig path is empty")
			}
			if outputDir != filepath.Dir(kubeconfigOut) {
				t.Errorf("output directory = %q, want %q", outputDir, filepath.Dir(kubeconfigOut))
			}
			return 1, nil
		},
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
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
	if !spireEntriesDumped {
		t.Error("runBootstrapSite did not dump SPIRE bootstrap entries")
	}
	body, readErr := os.ReadFile(kubeconfigOut)
	if readErr != nil {
		t.Fatalf("no kubeconfig written for the finished cluster: %v", readErr)
	}
	if !strings.Contains(string(body), "kind: Config") {
		t.Errorf("the written kubeconfig is not one: %q", body)
	}
}

func TestRunBootstrapSiteRetainsBootstrapClusterWhenSpireEntryDumpFails(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, errors.New("dump failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "writing SPIRE bootstrap resources") {
		t.Fatalf("error = %v, want SPIRE dump failure", err)
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after the SPIRE copy failed")
	}
}

func TestRunBootstrapSiteSkipsSpireEntryDumpWhenMTLSDisabled(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	dumpCalled := false
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		disableMTLS:         true,
		bootstrapRunner:     fakeBootstrapRunner{},
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			dumpCalled = true
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite returned error: %v", err)
	}
	if dumpCalled {
		t.Error("SPIRE entries were dumped with mTLS disabled")
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
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		clusterTimeout:      5 * time.Millisecond,
		kubeconfigOut:       filepath.Join(t.TempDir(), "unused.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
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
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           missingKubeconfigRunner{},
		kubeconfigTimeout:   5 * time.Millisecond,
		kubeconfigOut:       filepath.Join(t.TempDir(), "unused.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err == nil {
		t.Fatal("expected an error when the kubeconfig Secret is not published")
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after the site cluster became provisioned")
	}
	// The env prefix specifically, not just the command: without it the
	// operator's copy-pasted `kind delete cluster` resolves through kind's
	// Docker default, reports the cluster as missing, exits 0, and leaves the
	// cluster behind for the next run to collide with.
	for _, want := range []string{
		"not published",
		"retained for recovery",
		"KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// runtimeOnPath stands in for exec.LookPath finding the container runtime, so
// tests do not require a real nerdctl installation on the machine running
// `go test`. Tests about the preflight itself supply their own stub.
func runtimeOnPath(name string) (string, error) { return "/usr/local/bin/" + name, nil }

// setBootstrapEnv fills in the images `bootstrap site` requires, so a test
// about something else does not fail on one of them.
func setBootstrapEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "nvcr.io/org/controller:edge")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "cluster-api-provider-provisioned:latest")
}

func TestCWEDeployConfigUsesSiteSpecTrustDomain(t *testing.T) {
	setBootstrapEnv(t)
	t.Setenv("NKECTL_SPIFFE_TRUST_DOMAIN", "stale.example.com")

	cfg, err := cweDeployConfig(&sitespec.SiteSpec{
		Spire: sitespec.Spire{TrustDomain: "site.example.com"},
	}, "bootstrap-cluster", false, "", "")
	if err != nil {
		t.Fatalf("cweDeployConfig: %v", err)
	}
	if cfg.SPIFFETrustDomain != "site.example.com" {
		t.Errorf("SPIFFETrustDomain = %q, want site-spec value %q", cfg.SPIFFETrustDomain, "site.example.com")
	}
}

func TestCWEDeployConfigDefaultsTrustDomain(t *testing.T) {
	setBootstrapEnv(t)
	t.Setenv("NKECTL_SPIFFE_TRUST_DOMAIN", "")

	cfg, err := cweDeployConfig(&sitespec.SiteSpec{}, "bootstrap-cluster", false, "", "")
	if err != nil {
		t.Fatalf("cweDeployConfig: %v", err)
	}
	if cfg.SPIFFETrustDomain != "bootstrap-cluster.nke.local" {
		t.Errorf("SPIFFETrustDomain = %q, want derived value %q", cfg.SPIFFETrustDomain, "bootstrap-cluster.nke.local")
	}
}

// The image lookup must run against the runtime kind loads from, and a
// stale NKECTL_BOOTSTRAP_PROVIDER export must not steer it back to a Docker
// daemon a host is no longer required to have.
func TestRunBootstrapSiteUsesNerdctl(t *testing.T) {
	setBootstrapEnv(t)
	t.Setenv("NKECTL_BOOTSTRAP_PROVIDER", "docker")
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	_ = runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})

	if !slices.Contains(runner.names, bootstrapengine.RuntimeCLI) {
		t.Errorf("no %s call; ran %v", bootstrapengine.RuntimeCLI, runner.names)
	}
	if slices.Contains(runner.names, "docker") {
		t.Errorf("shelled out to docker; ran %v", runner.names)
	}
}

// A Docker-only host must be told what it is missing, through the command
// itself rather than through a unit-level call. Without the preflight the
// first failure is kind's — "failed to list clusters", several steps in,
// with the runtime named only in nested output — because kind execs nerdctl
// internally rather than this process doing it.
func TestRunBootstrapSiteRequiresNerdctlOnPath(t *testing.T) {
	setBootstrapEnv(t)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:        writeSiteSpec(t, validSiteSpec),
		bootstrapRunner: runner,
		cweRunner:       fakeCWERunner{},
		lookPath: func(name string) (string, error) {
			return "", fmt.Errorf("exec: %q: %w", name, exec.ErrNotFound)
		},
	})
	if err == nil {
		t.Fatal("expected an error when nerdctl is not on PATH")
	}
	if !strings.Contains(err.Error(), bootstrapengine.RuntimeCLI) {
		t.Errorf("error %q does not name the missing runtime", err)
	}
	if !strings.Contains(err.Error(), "Docker is not supported") {
		t.Errorf("error %q does not say Docker is unsupported", err)
	}
	// Before the cluster, not after: the point is to fail without having
	// stood anything up.
	if runner.sawArgs("create", "cluster") {
		t.Errorf("a missing runtime should be caught before the cluster is built; calls: %v", runner.calls)
	}
}

// An absent nerdctl is the first thing a Docker-only host hits, and pulling
// cannot fix it. The error has to name the requirement rather than blaming a
// registry login the operator has already done.
func TestEnsureImageAvailableDiagnosesMissingNerdctl(t *testing.T) {
	engine := bootstrapengine.New(runnerFunc(
		func(_ context.Context, _ map[string]string, _ string, _ ...string) (string, error) {
			return "", fmt.Errorf("exec %q: %w", bootstrapengine.RuntimeCLI, exec.ErrNotFound)
		},
	), nil)

	err := ensureImageAvailable(context.Background(), engine,
		&bootstrapengine.Cluster{Name: "c"}, "nvcr.io/org/controller:edge", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error when nerdctl is not on PATH")
	}
	if !strings.Contains(err.Error(), bootstrapengine.RuntimeCLI) {
		t.Errorf("error %q does not name the missing runtime", err)
	}
	if strings.Contains(err.Error(), "authentication failure") {
		t.Errorf("a missing binary was reported as an auth failure: %v", err)
	}
}

// runnerFunc adapts a function to bootstrapengine.CommandRunner.
type runnerFunc func(ctx context.Context, env map[string]string, name string, args ...string) (string, error)

func (f runnerFunc) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	return f(ctx, env, name, args...)
}

// The flag is gone outright rather than rejecting "docker" by value, so
// cobra's own unknown-flag error is the contract.
func TestBootstrapSiteRejectsBootstrapProviderFlag(t *testing.T) {
	cmd := newBootstrapCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"site", "-f", writeSiteSpec(t, validSiteSpec), "--bootstrap-provider", "docker"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --bootstrap-provider to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error %q does not report an unknown flag", err)
	}
}

// TestRunBootstrapSiteDefaultsImagesToTheLocalBuild confirms the bootstrap
// side-loads the local build's tags when neither image variable is set.
func TestRunBootstrapSiteDefaultsImagesToTheLocalBuild(t *testing.T) {
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "")
	t.Setenv("NKECTL_CWE_IMAGE", "")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "")
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       filepath.Join(t.TempDir(), "site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries:    func(context.Context, string, string) (int, error) { return 1, nil },
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err != nil {
		t.Fatalf("bootstrap with default images: %v", err)
	}
	for _, image := range []string{"nke-controller:latest", "cluster-api-provider-provisioned:latest"} {
		if !runner.sawArgs("image", "inspect", image) {
			t.Errorf("default image %s was not used; calls: %v", image, runner.calls)
		}
	}
}

// TestRunBootstrapSiteUsesTheImageFlags drives the two flags through the
// command: the images they name are the ones the bootstrap loads.
func TestRunBootstrapSiteUsesTheImageFlags(t *testing.T) {
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "env.example/nke-controller:env")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "env.example/provisioned:env")
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:                 writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:          runner,
		cweRunner:                fakeCWERunner{},
		kubeconfigOut:            filepath.Join(t.TempDir(), "site.kubeconfig"),
		lookPath:                 runtimeOnPath,
		dialContext:              refuseDial,
		untaintControlPlane:      skipUntaint,
		dumpSpireEntries:         func(context.Context, string, string) (int, error) { return 1, nil },
		encryptionKeyOut:         filepath.Join(t.TempDir(), "key.json"),
		controllerImage:          "nke-controller:flag",
		provisionedProviderImage: "cluster-api-provider-provisioned:flag",
	})
	if err != nil {
		t.Fatalf("bootstrap with image flags: %v", err)
	}
	for _, image := range []string{"nke-controller:flag", "cluster-api-provider-provisioned:flag"} {
		if !runner.sawArgs("image", "inspect", image) {
			t.Errorf("flag image %s was not used; calls: %v", image, runner.calls)
		}
	}
	for _, image := range []string{"env.example/nke-controller:env", "env.example/provisioned:env"} {
		if runner.sawArgs("image", "inspect", image) {
			t.Errorf("variable image %s was used although a flag named another; calls: %v", image, runner.calls)
		}
	}
}

// TestEnsureImageAvailableRefusesToPullAHostlessRef: a local build that is
// not in the image store fails with the build command and the flags and variables.
// Nothing is pulled, and the inspect failure stays in the message.
func TestEnsureImageAvailableRefusesToPullAHostlessRef(t *testing.T) {
	var pulled bool
	engine := bootstrapengine.New(runnerFunc(
		func(_ context.Context, _ map[string]string, _ string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "pull" {
				pulled = true
			}
			return "no such image: nke-controller:latest", errors.New("exit status 1")
		},
	), nil)

	err := ensureImageAvailable(context.Background(), engine,
		&bootstrapengine.Cluster{Name: "c"}, "nke-controller:latest", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error for a local build that is not in the store")
	}
	for _, want := range []string{"make -C cli images", "--controller-image", "NKECTL_CONTROLLER_IMAGE", "docker.io/<org>/<name>", "no such image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if pulled {
		t.Error("a reference without a registry was pulled")
	}
}

// TestRunBootstrapSiteRefusesMissingLocalImageBeforeCreate: with the default
// images and no local build, the command fails before any cluster is built.
func TestRunBootstrapSiteRefusesMissingLocalImageBeforeCreate(t *testing.T) {
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "")
	t.Setenv("NKECTL_CWE_IMAGE", "")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "")

	inner := runnerFunc(func(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "image" && args[1] == "inspect" && !hasRegistryHost(args[2]) {
			return "no such image: " + args[2], errors.New("exit status 1")
		}
		return fakeBootstrapRunner{}.Run(ctx, env, name, args...)
	})
	runner := &recordingRunner{next: inner}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err == nil {
		t.Fatal("expected an error when the default local image is not in the store")
	}
	for _, want := range []string{"nke-controller:latest", "make -C cli images", "--controller-image", "NKECTL_CONTROLLER_IMAGE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("a missing local image should be caught before the cluster is built; calls: %v", runner.calls)
	}
}

// TestMakefileImageTagsMatchTheManifest: the tags `make -C cli images` builds
// are the defaults `bootstrap site` looks for. Both come from the manifest;
// the Makefile copies them by hand, so this pins the copy.
func TestMakefileImageTagsMatchTheManifest(t *testing.T) {
	mk, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("reading cli/Makefile: %v", err)
	}
	m := manifest.Load()
	for _, tc := range []struct{ variable, component, image string }{
		{"CONTROLLER_IMAGE", manifest.ComponentController, manifest.ImageController},
		{"CAPI_PROVISIONED_IMAGE", manifest.ComponentProvisioned, manifest.ImageManager},
	} {
		re := regexp.MustCompile(`(?m)^` + tc.variable + `\s*\?=\s*(\S+)\s*$`)
		match := re.FindSubmatch(mk)
		if match == nil {
			t.Fatalf("cli/Makefile does not set %s", tc.variable)
		}
		want := m.Components[tc.component].Images[tc.image].Ref
		if got := string(match[1]); got != want {
			t.Errorf("cli/Makefile %s = %q, manifest says %q", tc.variable, got, want)
		}
	}
}

// TestMakefileControllerImageFallsBackToTheOldName drives `make -n images`:
// CONTROLLER_IMAGE wins, CWE_IMAGE is used with a warning when
// CONTROLLER_IMAGE is unset or empty, from the environment or the command
// line, and neither gives the manifest default.
func TestMakefileControllerImageFallsBackToTheOldName(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not on PATH")
	}
	const warning = "CWE_IMAGE is deprecated"
	for _, tc := range []struct {
		desc      string
		env, args []string
		wantTag   string
		wantWarn  bool
	}{
		{"neither", nil, nil, "nke-controller:latest", false},
		{"new name", nil, []string{"CONTROLLER_IMAGE=new:1"}, "new:1", false},
		{"old name from the environment", []string{"CWE_IMAGE=old:1"}, nil, "old:1", true},
		{"old name on the command line", nil, []string{"CWE_IMAGE=old:1"}, "old:1", true},
		{"both names", []string{"CWE_IMAGE=old:1"}, []string{"CONTROLLER_IMAGE=new:1"}, "new:1", false},
		{"old name with the new one empty on the command line", []string{"CWE_IMAGE=old:1"}, []string{"CONTROLLER_IMAGE="}, "old:1", true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			cmd := exec.Command(makeBin, append([]string{"-n", "-C", filepath.Join("..", ".."), "images"}, tc.args...)...)
			cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
				return strings.HasPrefix(kv, "CWE_IMAGE=") || strings.HasPrefix(kv, "CONTROLLER_IMAGE=")
			}), tc.env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make -n images: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "nerdctl build -t "+tc.wantTag+" ") {
				t.Errorf("controller image is not tagged %s:\n%s", tc.wantTag, out)
			}
			if got := strings.Contains(string(out), warning); got != tc.wantWarn {
				t.Errorf("deprecation warning printed = %v, want %v:\n%s", got, tc.wantWarn, out)
			}
		})
	}
}

// TestHasRegistryHost pins the rule: the first path component names a registry
// when it holds a dot or a colon, is localhost, or has an upper-case letter
// (the reference parser reads it as a domain). Docker Hub short names such as
// myorg/image:tag do not count; write docker.io/myorg/image:tag.
func TestHasRegistryHost(t *testing.T) {
	for ref, want := range map[string]bool{
		"nke-controller:latest":                   false,
		"cluster-api-provider-provisioned:latest": false,
		"nvcr.io/org/nke-controller:edge":         true,
		"localhost:5000/nke-controller:dev":       true,
		"localhost/nke-controller:dev":            true,
		"ghcr.io/spiffe/spire-server@sha256:abcd": true,
		"library/postgres:18.6":                   false,
		"MyRegistry/nke-controller:1":             true,
	} {
		if got := hasRegistryHost(ref); got != want {
			t.Errorf("hasRegistryHost(%q) = %v, want %v", ref, got, want)
		}
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

func writeSiteSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write site spec: %v", err)
	}
	return path
}

// TestDumpSiteSpecRedactsRegistryAPIKey covers the one branch in this package
// that exists purely to keep a credential out of the terminal. Nothing else
// asserted it: every other site spec in these tests omits apiKey, so the
// redaction line had an execution count of zero and a regression that printed
// the key verbatim would have passed.
func TestDumpSiteSpecRedactsRegistryAPIKey(t *testing.T) {
	const secret = "nvapi-do-not-print-me"

	inv, err := sitespec.Load(writeSiteSpec(t, validSiteSpec+"  apiKey: "+secret+"\n"))
	if err != nil {
		t.Fatalf("load site spec: %v", err)
	}
	if inv.Registry.APIKey != secret {
		t.Fatalf("fixture did not set the api key: got %q, want %q", inv.Registry.APIKey, secret)
	}

	var buf bytes.Buffer
	if err := dumpSiteSpec(&buf, inv); err != nil {
		t.Fatalf("dumpSiteSpec: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, secret) {
		t.Errorf("dumpSiteSpec printed the registry credential:\n%s", out)
	}
	if !strings.Contains(out, "***redacted***") {
		t.Errorf("dumpSiteSpec did not mark the credential as redacted:\n%s", out)
	}
	// Redaction works on a copy, so the caller's site spec must be left intact:
	// whatever runs next still needs the real credential.
	if inv.Registry.APIKey != secret {
		t.Errorf("dumpSiteSpec mutated the caller's site spec: api key is now %q", inv.Registry.APIKey)
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
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec:            writeSiteSpec(t, validSiteSpec),
			diagnosticsDir:      dir,
			bootstrapRunner:     fakeBootstrapRunner{},
			cweRunner:           fakeCWERunner{},
			kubeconfigOut:       filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
			lookPath:            runtimeOnPath,
			dialContext:         refuseDial,
			untaintControlPlane: skipUntaint,
			dumpSpireEntries: func(context.Context, string, string) (int, error) {
				return 0, nil
			},
			encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
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
		// waiting for a controller that will never report healthy.
		t.Setenv("NKECTL_CONTROLLER_READY_TIMEOUT", "1s")
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		// The controller never reports healthy: the failure this exists for.
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec:            writeSiteSpec(t, validSiteSpec),
			diagnosticsDir:      dir,
			bootstrapRunner:     fakeBootstrapRunner{},
			cweRunner:           unhealthyCWERunner{},
			lookPath:            runtimeOnPath,
			dialContext:         refuseDial,
			untaintControlPlane: skipUntaint,
			encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
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

// TestRunBootstrapSiteWaitsForEveryControlPlaneNode: a cluster reports
// Provisioned as soon as the one node it chose as the initial control-plane
// node has run kubeadm init. The other two join afterwards, through workflows
// that run in the bootstrap cluster this command deletes on its way out.
// Deleting it on the cluster condition alone strands whichever nodes had not
// finished, and a full re-bootstrap is the only way back.
//
// Both ends of the range are covered. One node joined is the reported bug: the
// command called a three-node cluster ready on the strength of the first node.
// Two joined is the straggler that is hardest to notice — the cluster is one
// node short of what was asked for, which is a quorum that works today and a
// control plane that cannot lose a node tomorrow.
func TestRunBootstrapSiteWaitsForEveryControlPlaneNode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		joined   int
		stranded []string
	}{
		{name: "only the initial node joined", joined: 1, stranded: []string{"node2", "node3"}},
		{name: "one node never joins", joined: 2, stranded: []string{"node3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBootstrapEnv(t)
			startFakeCWEAPIWithNodes(t, true, 3, tc.joined)

			runner := &recordingRunner{next: fakeBootstrapRunner{}}
			err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
				siteSpec:        writeSiteSpec(t, threeNodeSiteSpec),
				bootstrapRunner: runner,
				cweRunner:       fakeCWERunner{},
				// Longer than the 5ms the timeout-only tests above use. This
				// one asserts the error names how many nodes joined, so the
				// first read of the fake API has to land before the deadline.
				// At 5ms it did on Linux and did not on the macOS runner,
				// which reported "0 of 3". The deadline still ends the test.
				clusterTimeout:      500 * time.Millisecond,
				kubeconfigOut:       filepath.Join(t.TempDir(), "unused.kubeconfig"),
				lookPath:            runtimeOnPath,
				dialContext:         refuseDial,
				untaintControlPlane: skipUntaint,
				encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
			})
			if err == nil {
				t.Fatalf("bootstrap reported success with %d of 3 control plane nodes joined", tc.joined)
			}
			if runner.sawArgs("delete", "cluster") {
				t.Error("bootstrap kind cluster was torn down while a control plane node was still joining")
			}
			want := append([]string{
				fmt.Sprintf("%d of 3", tc.joined),
				"retained for recovery",
			}, tc.stranded...)
			for _, w := range want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			// The nodes that did finish are not stragglers, and naming them
			// would send the operator after the wrong host.
			for i := 1; i <= tc.joined; i++ {
				if node := fmt.Sprintf("node%d", i); strings.Contains(err.Error(), node) {
					t.Errorf("error %q lists %s, which had finished", err, node)
				}
			}
		})
	}
}

// TestRunBootstrapSiteTearsDownOnceEveryNodeHasJoined is the other half: the
// wait must end, and the ephemeral cluster must still be cleaned up.
func TestRunBootstrapSiteTearsDownOnceEveryNodeHasJoined(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPIWithNodes(t, true, 3, 3)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, threeNodeSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       filepath.Join(t.TempDir(), "nke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, nil
		},
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
	})
	if err != nil {
		t.Fatalf("runBootstrapSite returned error: %v", err)
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("runBootstrapSite did not tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
}

// fakeEtcdKey is the key material fakeCWERunner serves. Tests assert it
// reaches the file on disk and nothing else.
const fakeEtcdKey = "fake-etcd-key-material"

// missingEtcdKeyRunner fails the key Secret read with output that a leaking
// error would carry.
type missingEtcdKeyRunner struct{ fakeCWERunner }

const leakedEtcdKeyOutput = "OUTPUT-THAT-MUST-NOT-APPEAR"

func (r missingEtcdKeyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "kubectl" && strings.Contains(strings.Join(args, " "), "get secret etcd-encryption-key-") {
		return leakedEtcdKeyOutput, errors.New("exit status 1")
	}
	return r.fakeCWERunner.Run(ctx, name, args...)
}

// recordingCWERunner records every controllerdeploy command so a test can assert one
// was never issued.
type recordingCWERunner struct {
	next  controllerdeploy.CommandRunner
	calls []string
}

func (r *recordingCWERunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return r.next.Run(ctx, name, args...)
}

func (r *recordingCWERunner) saw(fragment string) bool {
	for _, call := range r.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// TestRunBootstrapSiteSavesTheEtcdEncryptionKey: the key the controller generated ends
// up on disk, as a Secret the operator can hand to a later controller, before
// the bootstrap cluster holding the controller's copy is deleted.
func TestRunBootstrapSiteSavesTheEtcdEncryptionKey(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	keyOut := filepath.Join(t.TempDir(), "site-key.json")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		kubeconfigOut:       filepath.Join(t.TempDir(), "site.kubeconfig"),
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}

	info, err := os.Stat(keyOut)
	if err != nil {
		t.Fatalf("the key file was not written: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key file mode = %o, want 0600", mode)
	}
	body, err := os.ReadFile(keyOut)
	if err != nil {
		t.Fatal(err)
	}
	var secret struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   *string           `json:"namespace"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(body, &secret); err != nil {
		t.Fatalf("key file is not a Secret manifest: %v", err)
	}
	if !strings.HasPrefix(secret.Metadata.Name, "etcd-encryption-key-") {
		t.Errorf("metadata.name = %q, want the controller's etcd-encryption-key-<uuid>", secret.Metadata.Name)
	}
	if secret.Metadata.Namespace != nil {
		t.Errorf("metadata.namespace = %q, want none", *secret.Metadata.Namespace)
	}
	if secret.Metadata.Annotations["nke.nvidia.com/cluster-name"] != "nke-site" {
		t.Errorf("cluster-name annotation = %q, want %q", secret.Metadata.Annotations["nke.nvidia.com/cluster-name"], "nke-site")
	}
	if string(secret.Data["active"]) != fakeEtcdKey {
		t.Errorf("data.active is not the key the controller served")
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("a successful bootstrap should still tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
}

// TestRunBootstrapSiteRetainsClusterWhenKeyCaptureFails: the cluster is up
// and encrypted, and the only copy of its key the operator does not have is in
// the bootstrap cluster. That cluster is kept, the message says how to get the
// key out, and it does not end — as the generic message does — by telling the
// operator to delete it.
func TestRunBootstrapSiteRetainsClusterWhenKeyCaptureFails(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	keyOut := filepath.Join(t.TempDir(), "key.json")
	kubeconfigOut := filepath.Join(t.TempDir(), "site.kubeconfig")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           missingEtcdKeyRunner{},
		kubeconfigOut:       kubeconfigOut,
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error when the key Secret cannot be read")
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted while it held the only reachable copy of the key")
	}
	// The recovery command writes what the tool would have: to the path the
	// operator chose, owner-readable only, the Secret the controller holds.
	for _, want := range []string{
		"was not saved to " + keyOut,
		"kubeconfig is at " + kubeconfigOut,
		"do not delete it",
		"umask 077 && kubectl --kubeconfig",
		"-n nke-controller get secret etcd-encryption-key-",
		"-o json > " + keyOut,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, unwanted := range []string{leakedEtcdKeyOutput, "kind delete cluster"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error %q contains %q", err, unwanted)
		}
	}
	if _, statErr := os.Stat(kubeconfigOut); statErr != nil {
		t.Errorf("the kubeconfig was not written before the key capture failed: %v", statErr)
	}
}

// TestRequireNoKeyFile: only a path that does not exist is free. Anything that
// cannot be checked would fail the write at the very end of a bootstrap.
func TestRequireNoKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := requireNoKeyFile(filepath.Join(dir, "absent.json")); err != nil {
		t.Errorf("absent path: err = %v, want nil", err)
	}

	existing := filepath.Join(dir, "existing.json")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireNoKeyFile(existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing path: err = %v, want \"already exists\"", err)
	}

	// A regular file where a directory is needed: Lstat fails with ENOTDIR,
	// which is not "free".
	if err := requireNoKeyFile(filepath.Join(existing, "key.json")); err == nil || !strings.Contains(err.Error(), "checking") {
		t.Errorf("unwritable path: err = %v, want a check failure", err)
	}
}

// TestRunBootstrapSiteSkipsKeyCaptureWhenEncryptionIsOff: with encryption
// off there is no Secret to read, and a read that waited for one would fail a
// healthy bootstrap.
func TestRunBootstrapSiteSkipsKeyCaptureWhenEncryptionIsOff(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	keyOut := filepath.Join(t.TempDir(), "key.json")
	cwe := &recordingCWERunner{next: fakeCWERunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, strings.Replace(validSiteSpec, "  k8sVersion:", "  etcdEncryption: false\n  k8sVersion:", 1)),
		bootstrapRunner:     &recordingRunner{next: fakeBootstrapRunner{}},
		cweRunner:           cwe,
		kubeconfigOut:       filepath.Join(t.TempDir(), "site.kubeconfig"),
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}
	if cwe.saw("etcd-encryption-key-") {
		t.Errorf("the key Secret was read for an unencrypted cluster; calls: %v", cwe.calls)
	}
	if _, statErr := os.Stat(keyOut); statErr == nil {
		t.Error("a key file was written for an unencrypted cluster")
	}
}

// TestRunBootstrapSiteRefusesAnExistingKeyFile: the file is never overwritten,
// and a run that would fail on that at the very end is refused at the start.
func TestRunBootstrapSiteRefusesAnExistingKeyFile(t *testing.T) {
	setBootstrapEnv(t)

	keyOut := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(keyOut, []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error when the key file already exists")
	}
	for _, want := range []string{keyOut, "already exists", "delete the file and re-run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("the bootstrap kind cluster was created despite an existing key file; calls: %v", runner.calls)
	}
}

// skipUntaint keeps tests that are not about the untaint from building a
// client out of the fake kubeconfig.
func skipUntaint(context.Context, string, *slog.Logger) ([]string, error) { return nil, nil }

// refuseDial stands in for a site spec host with nothing on the API port, so
// the pre-create probe passes without dialling anything real.
func refuseDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("connection refused")
}

// TestRunBootstrapSiteRefusesHostsServingAControlPlane: a host that already
// answers on the API port is not provisioned over. The message names the host
// and the way out, and nothing has been built by the time it is printed.
func TestRunBootstrapSiteRefusesHostsServingAControlPlane(t *testing.T) {
	setBootstrapEnv(t)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:         writeSiteSpec(t, threeNodeSiteSpec),
		bootstrapRunner:  runner,
		cweRunner:        fakeCWERunner{},
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		lookPath:         runtimeOnPath,
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			if address != "10.0.0.2:6443" {
				return nil, errors.New("connection refused")
			}
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	})
	if err == nil {
		t.Fatal("expected an error when a site spec host already serves a control plane")
	}
	for _, want := range []string{"node2 (10.0.0.2)", "6443", "kubeadm reset -f"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "node1") || strings.Contains(err.Error(), "node3") {
		t.Errorf("error %q names a host that refused the connection", err)
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("the bootstrap kind cluster was created despite a host already serving a control plane; calls: %v", runner.calls)
	}
}

// leftoverKindRunner reports the bootstrap cluster as already present, as a
// run that kept it — or failed and retained it — leaves things.
type leftoverKindRunner struct{ fakeBootstrapRunner }

func (r leftoverKindRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	if name == "kind" && len(args) >= 2 && args[0] == "get" && args[1] == "clusters" {
		return "nkectl-bootstrap\n", nil
	}
	return r.fakeBootstrapRunner.Run(ctx, env, name, args...)
}

// TestRunBootstrapSiteRefusesWhileAnEarlierBootstrapClusterRuns: the cluster
// left by an earlier run may hold the key that run did not save. The refusal
// says how to get it out before it says how to delete the cluster, and does
// not delete or reuse anything itself.
func TestRunBootstrapSiteRefusesWhileAnEarlierBootstrapClusterRuns(t *testing.T) {
	setBootstrapEnv(t)

	runner := &recordingRunner{next: leftoverKindRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           fakeCWERunner{},
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error while an earlier bootstrap cluster is still running")
	}
	msg := err.Error()
	for _, want := range []string{
		"still running",
		"kind get kubeconfig --name nkectl-bootstrap",
		"etcd-encryption-key-",
		"get secrets -A | grep etcd-encryption-key-",
		"kind delete cluster --name nkectl-bootstrap",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Index(msg, "get kubeconfig") > strings.Index(msg, "delete cluster") {
		t.Errorf("error %q tells the operator to delete before it tells them to save the key", msg)
	}
	if runner.sawArgs("create", "cluster") || runner.sawArgs("delete", "cluster") {
		t.Errorf("the check must neither create nor delete a bootstrap cluster; calls: %v", runner.calls)
	}
}

// TestRunBootstrapSiteRemovesTheControlPlaneTaint: the untaint runs by
// default, against the kubeconfig the command just wrote.
func TestRunBootstrapSiteRemovesTheControlPlaneTaint(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nke-site.kubeconfig")
	untaintedWith := ""
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:         writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:  &recordingRunner{next: fakeBootstrapRunner{}},
		cweRunner:        fakeCWERunner{},
		kubeconfigOut:    kubeconfigOut,
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		lookPath:         runtimeOnPath,
		dialContext:      refuseDial,
		dumpSpireEntries: func(context.Context, string, string) (int, error) { return 0, nil },
		untaintControlPlane: func(_ context.Context, kubeconfigPath string, _ *slog.Logger) ([]string, error) {
			untaintedWith = kubeconfigPath
			return []string{"node1"}, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}
	if untaintedWith != kubeconfigOut {
		t.Errorf("untaint ran against %q, want the written kubeconfig %q", untaintedWith, kubeconfigOut)
	}
}

// TestRunBootstrapSiteReportsAnUntaintFailure: the untaint is the last step,
// so everything else is on disk and already reported when it fails, and the
// bootstrap cluster is neither dumped nor kept.
func TestRunBootstrapSiteReportsAnUntaintFailure(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nke-site.kubeconfig")
	keyOut := filepath.Join(t.TempDir(), "key.json")
	spireEntriesDumped := false
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	cwe := &recordingCWERunner{next: fakeCWERunner{}}
	var out bytes.Buffer
	err := runBootstrapSite(context.Background(), &out, &bootstrapSiteOptions{
		siteSpec:         writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:  runner,
		cweRunner:        cwe,
		kubeconfigOut:    kubeconfigOut,
		encryptionKeyOut: keyOut,
		lookPath:         runtimeOnPath,
		dialContext:      refuseDial,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			spireEntriesDumped = true
			return 1, nil
		},
		untaintControlPlane: func(context.Context, string, *slog.Logger) ([]string, error) {
			return nil, errors.New("nodes is forbidden")
		},
	})
	if err == nil {
		t.Fatal("runBootstrapSite returned nil, want the untaint failure")
	}
	if !spireEntriesDumped {
		t.Error("the SPIRE export did not run; the untaint must come after it")
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("the bootstrap kind cluster was kept after the untaint failed, though nothing in it is needed; calls: %v", runner.calls)
	}
	if cwe.saw("all,events") {
		t.Errorf("bootstrap cluster diagnostics were collected for a failure in the site cluster; calls: %v", cwe.calls)
	}
	for _, want := range []string{"nodes is forbidden", "kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule-", kubeconfigOut} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, path := range []string{kubeconfigOut, keyOut} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("%s is missing after the untaint failure: %v", path, statErr)
		}
	}
	if !strings.Contains(out.String(), keyOut) {
		t.Errorf("the etcd key location was not reported before the untaint; output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "is ready") {
		t.Errorf("the cluster was reported ready although the untaint failed; output:\n%s", out.String())
	}
}

// TestMakeControlPlaneSchedulableBuildsTheClientFromTheKubeconfig covers the
// production path, with a kubeconfig that cannot be loaded.
func TestMakeControlPlaneSchedulableBuildsTheClientFromTheKubeconfig(t *testing.T) {
	inv, err := sitespec.Load(writeSiteSpec(t, validSiteSpec))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.kubeconfig")

	err = makeControlPlaneSchedulable(context.Background(), inv, missing, &bootstrapSiteOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("makeControlPlaneSchedulable returned nil for a kubeconfig that does not exist")
	}
	for _, want := range []string{missing, "kubectl taint nodes --all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestCWEDeployConfigImagePrecedence: a flag wins over its variable, and a
// variable wins over the manifest default.
func TestCWEDeployConfigImagePrecedence(t *testing.T) {
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "env.example/nke-controller:env")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "env.example/provisioned:env")

	cfg, err := cweDeployConfig(&sitespec.SiteSpec{}, "c", false, "flag.example/nke-controller:flag", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CWEImage != "flag.example/nke-controller:flag" {
		t.Errorf("CWEImage = %q, want the flag", cfg.CWEImage)
	}
	if cfg.ProvisionedProviderImage != "env.example/provisioned:env" {
		t.Errorf("ProvisionedProviderImage = %q, want the variable", cfg.ProvisionedProviderImage)
	}

	cfg, err = cweDeployConfig(&sitespec.SiteSpec{}, "c", false, "", "flag.example/provisioned:flag")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CWEImage != "env.example/nke-controller:env" {
		t.Errorf("CWEImage = %q, want the variable", cfg.CWEImage)
	}
	if cfg.ProvisionedProviderImage != "flag.example/provisioned:flag" {
		t.Errorf("ProvisionedProviderImage = %q, want the flag", cfg.ProvisionedProviderImage)
	}
}

// TestControllerImageOrder pins the whole order for the
// controller image: the flag, then NKECTL_CONTROLLER_IMAGE, then the old
// NKECTL_CWE_IMAGE, then the manifest default.
func TestControllerImageOrder(t *testing.T) {
	for _, tc := range []struct {
		desc, flag, newV, oldV, want string
	}{
		{"flag over both settings", "flag.example/c:flag", "new.example/c:new", "old.example/c:old", "flag.example/c:flag"},
		{"new setting over old", "", "new.example/c:new", "old.example/c:old", "new.example/c:new"},
		{"old setting alone", "", "", "old.example/c:old", "old.example/c:old"},
		{"neither", "", "", "", "nke-controller:latest"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("NKECTL_CONTROLLER_IMAGE", tc.newV)
			t.Setenv("NKECTL_CWE_IMAGE", tc.oldV)
			cfg, err := cweDeployConfig(&sitespec.SiteSpec{}, "c", false, tc.flag, "")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CWEImage != tc.want {
				t.Errorf("CWEImage = %q, want %q", cfg.CWEImage, tc.want)
			}
		})
	}
}

// TestRunBootstrapSiteWarnsAboutOldSettingNames: an old NKECTL_CWE_* name set
// without its replacement is logged as a warning naming the replacement, before
// anything else runs; a setting under its new name is not.
func TestRunBootstrapSiteWarnsAboutOldSettingNames(t *testing.T) {
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "")
	t.Setenv("NKECTL_CWE_IMAGE", "old.example/c:old")
	t.Setenv("NKECTL_CONTROLLER_HELM_TIMEOUT", "2m")
	t.Setenv("NKECTL_CWE_HELM_TIMEOUT", "")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A missing site spec stops the command right after the warnings.
	_ = runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec: filepath.Join(t.TempDir(), "missing.yaml"),
	})

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") ||
		!strings.Contains(logged, "NKECTL_CWE_IMAGE is deprecated and will be removed in a future release; set NKECTL_CONTROLLER_IMAGE instead") {
		t.Errorf("no deprecation warning for NKECTL_CWE_IMAGE in:\n%s", logged)
	}
	if strings.Contains(logged, "HELM_TIMEOUT") {
		t.Errorf("warned about a setting given under its new name:\n%s", logged)
	}
}
