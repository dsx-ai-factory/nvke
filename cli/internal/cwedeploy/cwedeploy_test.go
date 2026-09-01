// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwedeploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCall records one CommandRunner.Run invocation.
type fakeCall struct {
	name string
	args []string
}

// fakeRunner is a scripted CommandRunner for unit tests: no real helm,
// kubectl or cluster required.
type fakeRunner struct {
	calls []fakeCall

	// outputs is consulted in call order; if shorter than the number of
	// calls, the last entry is reused.
	outputs []fakeOutput

	// applied holds the contents of every manifest a kubectl apply was
	// pointed at. See captureApply.
	applied []string
}

type fakeOutput struct {
	output string
	err    error
}

// appliedManifests holds the contents of every file a `kubectl apply -f` was
// pointed at, captured while the file still exists: the callers remove theirs
// as soon as the apply returns, so a test that looked afterwards would find
// nothing. This is the only way to assert on what was actually sent to the
// cluster rather than on the command line that sent it.
func (f *fakeRunner) captureApply(name string, args []string) {
	if name != "kubectl" {
		return
	}
	for i, arg := range args {
		if arg == "-f" && i+1 < len(args) {
			body, err := os.ReadFile(args[i+1])
			if err == nil {
				f.applied = append(f.applied, string(body))
			}
			return
		}
	}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	f.captureApply(name, args)
	idx := len(f.calls) - 1
	if idx < len(f.outputs) {
		return f.outputs[idx].output, f.outputs[idx].err
	}
	if len(f.outputs) > 0 {
		last := f.outputs[len(f.outputs)-1]
		return last.output, last.err
	}
	// `kubectl kustomize` is the one call whose stdout Deploy reads rather than
	// discards: it renders the provisioned provider and rewrites its image. An
	// empty answer is not a manifest, so stand in for one here rather than
	// making every test script it.
	if name == "kubectl" && len(args) > 0 && args[0] == "kustomize" {
		return "        image: " + provisionedDefaultImage + "\n", nil
	}
	// The CAPI probe must report "not installed", or Deploy skips clusterctl.
	if name == "kubectl" && containsArg(args, "deployment") && containsArg(args, capiCoreDeployment) {
		return "Error from server (NotFound)", errors.New("exit status 1")
	}
	return "", nil
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func joinArgs(args []string) string { return strings.Join(args, " ") }

// helmCalls returns just the helm invocations, in order. Deploy also shells out
// to kubectl and clusterctl, so a test about which releases are installed, and
// in which order, has to say so rather than index into every call.
func helmCalls(runner *fakeRunner) []string {
	var out []string
	for _, call := range runner.calls {
		if call.name == "helm" {
			out = append(out, joinArgs(call.args))
		}
	}
	return out
}

// callIndex returns the position of the first call whose arguments contain
// want, or -1. Used to assert ordering between calls to different binaries.
func callIndex(runner *fakeRunner, want string) int {
	for i, call := range runner.calls {
		if strings.Contains(joinArgs(call.args), want) {
			return i
		}
	}
	return -1
}

// testConfig is a Config with every required field filled in, so a test that
// cares about one of them can set just that one.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		CWEImage:                 "nvcr.io/org/controller:edge",
		ProfilesImage:            "nvcr.io/org/cwe-profiles:v1",
		ProvisionedProviderImage: "cluster-api-provider-provisioned:local",
		ProvisionedProviderPath:  t.TempDir(),
		ChartPath:                t.TempDir(), // any existing dir stands in for the chart
		PostgresPassword:         "test-password",
	}
}

func TestDeploy_RequiresKubeconfigAndImage(t *testing.T) {
	e := New(&fakeRunner{}, nil)

	if err := e.Deploy(context.Background(), "", Config{CWEImage: "nvcr.io/org/controller:edge"}); err == nil {
		t.Fatal("expected an error for a missing kubeconfigPath")
	}
	if err := e.Deploy(context.Background(), "/tmp/kubeconfig", Config{}); err == nil {
		t.Fatal("expected an error for a missing Config.CWEImage")
	}
	// Without profiles CWE fails its schema migration, so say so up front
	// rather than after a five-minute wait on a pod that will never be ready.
	if err := e.Deploy(context.Background(), "/tmp/kubeconfig",
		Config{CWEImage: "nvcr.io/org/controller:edge"}); err == nil {
		t.Fatal("expected an error for a missing Config.ProfilesImage")
	}
}

func TestDeploy_InstallsPostgresTemporalSPIREThenCWEInOrder(t *testing.T) {
	runner := &fakeRunner{}
	e := New(runner, nil)

	cfg := testConfig(t)
	cfg.SPIFFEClusterName = "test-kind"
	cfg.SPIFFETrustDomain = "test-kind.nke.local"
	if err := e.Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	releases := helmCalls(runner)
	if len(releases) != 5 {
		t.Fatalf("expected 5 helm calls (postgres, temporal, SPIRE CRDs, SPIRE, cwe), got %d: %+v",
			len(releases), releases)
	}

	for i, call := range runner.calls {
		if call.name != "helm" {
			continue
		}
		if joinArgs(call.args) == "" || call.args[0] != "--kubeconfig" || call.args[1] != "/tmp/kubeconfig" {
			t.Errorf("call[%d] missing --kubeconfig /tmp/kubeconfig: %s", i, joinArgs(call.args))
		}
	}

	postgres, temporal, spireCRDs, spire, cwe := releases[0], releases[1], releases[2], releases[3], releases[4]

	if !strings.Contains(postgres, "upgrade --install postgres "+postgresChartRef) {
		t.Errorf("postgres call = %q, want the postgres release/chart", postgres)
	}
	if !strings.Contains(postgres, "auth.password=test-password") {
		t.Errorf("postgres call = %q, want the postgres password set", postgres)
	}

	if !strings.Contains(temporal, "upgrade --install temporal temporal --repo "+temporalChartRepo) {
		t.Errorf("temporal call = %q, want the temporal release/chart/repo", temporal)
	}
	if !strings.Contains(temporal, "server.config.persistence.default.sql.password=test-password") {
		t.Errorf("temporal call = %q, want the shared postgres password set on both persistence stores", temporal)
	}

	if !strings.Contains(spireCRDs, "upgrade --install spire-crds "+spireCRDsChartName) ||
		!strings.Contains(spireCRDs, "--repo "+spireChartRepo) {
		t.Errorf("SPIRE CRDs call = %q, want the pinned CRD chart", spireCRDs)
	}
	if !strings.Contains(spire, "upgrade --install spire "+spireChartName) ||
		!strings.Contains(spire, "global.spire.clusterName=test-kind") ||
		!strings.Contains(spire, "global.spire.trustDomain=test-kind.nke.local") ||
		!strings.Contains(spire, "spire-server.adminIDs[0]=spiffe://test-kind.nke.local/nke-service/cloud-workflow-engine") {
		t.Errorf("SPIRE call = %q, want the pinned chart and CWE identity", spire)
	}

	if !strings.Contains(cwe, "upgrade --install cloud-workflow-engine "+cfg.ChartPath) {
		t.Errorf("cwe call = %q, want the cwe release/chart path", cwe)
	}
	if !strings.Contains(cwe, "image.repository=nvcr.io/org/controller") || !strings.Contains(cwe, "image.tag=edge") {
		t.Errorf("cwe call = %q, want the image split into repository/tag", cwe)
	}
	if !strings.Contains(cwe, "profiles.image.enabled=true") ||
		!strings.Contains(cwe, "profiles.image.repository=nvcr.io/org/cwe-profiles") ||
		!strings.Contains(cwe, "profiles.path="+profilesPath) {
		t.Errorf("cwe call = %q, want the profiles init container enabled and pointed at the image", cwe)
	}
	if !strings.Contains(cwe, "spiffe.enabled=true") ||
		!strings.Contains(cwe, "spiffe.trustDomain=test-kind.nke.local") ||
		!strings.Contains(cwe, "spiffe.spire.serverAddress="+spireServiceHost()) ||
		!strings.Contains(cwe, "spiffe.spire.serverPort="+spireServerPort) {
		t.Errorf("cwe call = %q, want SPIFFE enabled with the SPIRE trust domain", cwe)
	}
}

func TestDeploy_PropagatesHelmError(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		{output: "Error: INSTALLATION FAILED", err: errors.New("exit status 1")},
	}}
	e := New(runner, nil)

	err := e.Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t))
	if err == nil {
		t.Fatal("expected Deploy to propagate the helm error")
	}
	if !strings.Contains(err.Error(), "installing postgres") {
		t.Errorf("error %q does not identify which release failed", err)
	}
	if !strings.Contains(err.Error(), "INSTALLATION FAILED") {
		t.Errorf("error %q does not include helm's output", err)
	}
}

func TestDeploy_ResolvesChartPathFromRepoRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chartDir := filepath.Join(root, "controller", "deploy", "charts", "cloud-workflow-engine")
	if err := os.MkdirAll(chartDir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "cli", "cmd", "nkectl")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	got, err := resolveChartPath()
	if err != nil {
		t.Fatalf("resolveChartPath returned error: %v", err)
	}
	// Resolve symlinks on both sides: on macOS, t.TempDir() lives under
	// /tmp, which is itself a symlink to /private/tmp, and os.Getwd() (which
	// resolveChartPath walks up from) returns the resolved path.
	wantResolved, err := filepath.EvalSymlinks(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotResolved != wantResolved {
		t.Errorf("resolveChartPath() = %q, want %q", gotResolved, wantResolved)
	}
}

func TestDeploy_ChartPathResolutionFailsOutsideARepo(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	if _, err := resolveChartPath(); err == nil {
		t.Fatal("expected an error when no go.work is found above the working directory")
	}
}

func TestWaitReady_SucceedsOnOKStatus(t *testing.T) {
	old := waitReadyPollInterval
	waitReadyPollInterval = time.Millisecond
	t.Cleanup(func() { waitReadyPollInterval = old })

	runner := &fakeRunner{outputs: []fakeOutput{
		{output: `{"status":"unavailable","checks":{"database":"ok","temporal":"connection refused"}}`, err: errors.New("exit status 1")},
		{output: `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`},
	}}
	e := New(runner, nil)

	err := e.WaitReady(context.Background(), "/tmp/kubeconfig", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitReady returned error: %v", err)
	}
	if len(runner.calls) < 1 {
		t.Fatal("expected at least one kubectl call")
	}
	call := runner.calls[0]
	if call.name != "kubectl" {
		t.Errorf("call.name = %q, want %q", call.name, "kubectl")
	}
	wantProxyPath := "/api/v1/namespaces/cloud-workflow-engine/services/cloud-workflow-engine:http/proxy/readyz"
	if !strings.Contains(joinArgs(call.args), wantProxyPath) {
		t.Errorf("args = %q, want it to include %q", joinArgs(call.args), wantProxyPath)
	}
}

func TestWaitReady_TimesOutWithLastResponseInError(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		{output: `{"status":"unavailable","checks":{"temporal":"connection refused"}}`, err: errors.New("exit status 1")},
	}}
	e := New(runner, nil)

	err := e.WaitReady(context.Background(), "/tmp/kubeconfig", 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected WaitReady to time out")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not include the last observed response", err)
	}
}

func TestSplitImageRef(t *testing.T) {
	const digest = "sha256:8090c7c4538af8e717d52f5edcf26eaf4a4afae2d3e9a25c8208b0704934954e"

	tests := []struct {
		ref        string
		wantRepo   string
		wantTag    string
		wantDigest string
	}{
		{"nvcr.io/org/controller:edge", "nvcr.io/org/controller", "edge", ""},
		{"nvcr.io/org/controller:sha-abc123", "nvcr.io/org/controller", "sha-abc123", ""},
		{"localhost:5000/controller:edge", "localhost:5000/controller", "edge", ""},
		{"nvcr.io/org/controller", "nvcr.io/org/controller", "latest", ""},
		// The colon inside "sha256:..." must not read as a tag separator,
		// which would leave a repository ending in "@sha256".
		{"ghcr.io/nvidia/nke/controller@" + digest, "ghcr.io/nvidia/nke/controller", "", digest},
		{"ghcr.io/nvidia/nke/controller:v0.1.1-rc.2@" + digest, "ghcr.io/nvidia/nke/controller", "v0.1.1-rc.2", digest},
		{"localhost:5000/controller@" + digest, "localhost:5000/controller", "", digest},
	}
	for _, tt := range tests {
		repo, tag, dgst := splitImageRef(tt.ref)
		if repo != tt.wantRepo || tag != tt.wantTag || dgst != tt.wantDigest {
			t.Errorf("splitImageRef(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tt.ref, repo, tag, dgst, tt.wantRepo, tt.wantTag, tt.wantDigest)
		}
	}
}

// A digest must reach helm as image.digest. As image.tag it would render
// "repo:sha256:...", which Kubernetes cannot pull.
func TestDeploy_PinsImageByDigest(t *testing.T) {
	const digest = "sha256:8090c7c4538af8e717d52f5edcf26eaf4a4afae2d3e9a25c8208b0704934954e"

	runner := &fakeRunner{}
	cfg := testConfig(t)
	cfg.CWEImage = "ghcr.io/nvidia/nke/controller@" + digest
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	releases := helmCalls(runner)
	cwe := releases[len(releases)-1]
	if !strings.Contains(cwe, "image.repository=ghcr.io/nvidia/nke/controller") ||
		!strings.Contains(cwe, "image.digest="+digest) {
		t.Errorf("cwe call = %q, want the digest split into repository/digest", cwe)
	}
}

// TestDumpDiagnostics covers the failure-path bundle: it must collect the
// cluster dump plus the extras that explain a pod which never started, and it
// must not turn a diagnostics problem into the error the caller sees.
func TestDumpDiagnostics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
	runner := &fakeRunner{outputs: []fakeOutput{{output: "collected"}}}

	New(runner, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	var dumped bool
	for _, call := range runner.calls {
		if strings.Contains(joinArgs(call.args), "cluster-info dump") &&
			strings.Contains(joinArgs(call.args), "--output-directory "+dir) {
			dumped = true
		}
	}
	if !dumped {
		t.Errorf("no cluster-info dump into %s; calls were %+v", dir, runner.calls)
	}
	for _, name := range []string{"all-resources.txt", "pods-described.txt", "helm-releases.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// TestDumpDiagnostics_SurvivesAFailingCluster is the property that matters on
// the path this runs on: every command failing must still leave a bundle and
// must not panic, because the caller already has a real error to report.
func TestDumpDiagnostics_SurvivesAFailingCluster(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
	runner := &fakeRunner{outputs: []fakeOutput{{output: "unreachable", err: errors.New("exit status 1")}}}

	New(runner, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	body, err := os.ReadFile(filepath.Join(dir, "all-resources.txt"))
	if err != nil {
		t.Fatalf("no bundle written when every command failed: %v", err)
	}
	if !strings.Contains(string(body), "exit status 1") {
		t.Errorf("the file should record why the command failed, got %q", body)
	}
}

// TestSpireInstallArgs pins what nkectl computes rather than what the values
// file carries: the chart it resolves, and the depth the computed values are
// set at. A value set at the wrong depth is silently ignored by helm rather
// than rejected, so nothing else would catch it.
func TestSpireInstallArgs(t *testing.T) {
	got := joinArgs(spireInstallArgs(Config{}, "/tmp/values.yaml", "kind", "kind.nke.local", time.Minute))
	for _, want := range []string{
		"upgrade --install spire " + spireChartName,
		"--repo " + spireChartRepo,
		"--version " + spireChartVersion,
		"--values /tmp/values.yaml",
		"global.spire.clusterName=kind",
		"global.spire.trustDomain=kind.nke.local",
		spireServerValuesPrefix + ".adminIDs[0]=spiffe://kind.nke.local/nke-service/cloud-workflow-engine",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("install = %q, want it to contain %q", got, want)
		}
	}
}

// TestUpstreamCAArgs covers both states: a CA from the inventory is chained to,
// and no CA leaves SPIRE self-signed rather than half-configured.
func TestUpstreamCAArgs(t *testing.T) {
	if got := upstreamCAArgs(Config{}, "spire-server"); len(got) != 0 {
		t.Errorf("upstreamCAArgs with no CA = %v, want none", got)
	}
	// A cert without a key is not a CA; neither half alone should configure one.
	if got := upstreamCAArgs(Config{SPIFFEUpstreamCACertPath: "/ca.crt"}, "spire-server"); len(got) != 0 {
		t.Errorf("upstreamCAArgs with no key = %v, want none", got)
	}

	got := joinArgs(upstreamCAArgs(Config{
		SPIFFEUpstreamCACertPath: "/ca.crt",
		SPIFFEUpstreamCAKeyPath:  "/ca.key",
	}, "spire-server"))
	for _, want := range []string{
		"spire-server.upstreamAuthority.disk.enabled=true",
		"--set-file spire-server.upstreamAuthority.disk.secret.data.certificate=/ca.crt",
		"--set-file spire-server.upstreamAuthority.disk.secret.data.key=/ca.key",
		// A self-signed root is its own bundle; without this SPIRE publishes
		// no trust anchor for the agents that already hold it.
		"--set-file spire-server.upstreamAuthority.disk.secret.data.bundle=/ca.crt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("upstreamCAArgs = %q, want it to contain %q", got, want)
		}
	}
}

// TestTPMAttestorArgs covers the two states that matter: an allowlist turns the
// attestor on, and no allowlist leaves it off.
func TestTPMAttestorArgs(t *testing.T) {
	// Reaching the server from outside is the bundle's external-service
	// manifest, not a chart value, so nothing here should touch the Service.
	if strings.Contains(joinArgs(tpmAttestorArgs(
		Config{TPMEKFingerprints: []string{"aaa"}}, "spire-server")), "service.type") {
		t.Error("tpmAttestorArgs should not change the chart's Service")
	}

	none := tpmAttestorArgs(Config{}, "spire-server")
	if len(none) != 0 {
		t.Errorf("tpmAttestorArgs with no fingerprints = %v, want none: a bootstrap that "+
			"has not been told which hardware to admit must admit none", none)
	}

	got := joinArgs(tpmAttestorArgs(
		Config{TPMEKFingerprints: []string{"aaa", "bbb"}}, "spire.spire-server"))
	for _, want := range []string{
		"spire.spire-server.nodeAttestor.tpmDirect.enabled=true",
		"spire.spire-server.nodeAttestor.tpmDirect.hashes[0]=aaa",
		"spire.spire-server.nodeAttestor.tpmDirect.hashes[1]=bbb",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tpmAttestorArgs = %q, want it to contain %q", got, want)
		}
	}
}

// TestSpireAndCWEAgreeOnTheClassName is the guard for a mismatch neither side
// reports. CWE stamps its ClusterStaticEntry objects with its configured class
// and the SPIRE controller manager only acts on objects carrying its own, so
// two different strings mean CWE writes entries nobody reads and a node never
// gets an identity -- with both components healthy and nothing logged.
//
// Neither default reaches the same string on its own: the chart derives
// <namespace>-<release>, CWE defaults to spire-mgmt-spire.
//
// The expected class is spelled out rather than taken from the constant, for
// the same reason the port test spells out its address: asserting the constant
// against itself would survive any drift. This pins our value to the literal
// CWE's SPIRE_CLASS_NAME defaults to. It cannot catch CWE changing that default
// -- that lives in another module -- but it does catch this side moving.
func TestSpireAndCWEAgreeOnTheClassName(t *testing.T) {
	const cweClassNameDefault = "spire-mgmt-spire"

	if spireControllerManagerClass != cweClassNameDefault {
		t.Errorf("the class is %q, but CWE's SPIRE_CLASS_NAME defaults to %q; "+
			"CWE would write ClusterStaticEntry objects the controller manager ignores",
			spireControllerManagerClass, cweClassNameDefault)
	}

	runner := &fakeRunner{}
	cfg := testConfig(t)
	cfg.SPIFFEClusterName = "test-kind"
	cfg.SPIFFETrustDomain = "test-kind.nke.local"
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	var spire, cwe string
	for _, call := range runner.calls {
		args := joinArgs(call.args)
		switch {
		case strings.Contains(args, "upgrade --install "+spireReleaseName+" "+spireChartName):
			spire = args
		case strings.Contains(args, "upgrade --install "+cweReleaseName):
			cwe = args
		}
	}
	if spire == "" || cwe == "" {
		t.Fatalf("expected both installs; spire=%q cwe=%q", spire, cwe)
	}

	if !strings.Contains(spire,
		spireServerValuesPrefix+".controllerManager.className="+spireControllerManagerClass) {
		t.Errorf("SPIRE install does not pin the controller-manager class: %s", spire)
	}
	if !strings.Contains(cwe, "spiffe.spire.className="+spireControllerManagerClass) {
		t.Errorf("CWE install does not pin the same class: %s", cwe)
	}
}

// TestSpireServesThePortCWEExpects covers the other silent mismatch: CWE reads
// SPIRE_SERVER_API_ADDRESS for join-token calls and nkectl never sets it, so
// the Service has to listen where that default already points. The literal is
// spelled out rather than built from spireServerPort, because the thing under
// test is that our constant still matches CWE's default -- writing both sides
// from the same constant would assert nothing.
func TestSpireServesThePortCWEExpects(t *testing.T) {
	const cweAPIAddressDefault = "spire-server.spire-server.svc.cluster.local:8081"

	if got := spireServiceHost() + ":" + spireServerPort; got != cweAPIAddressDefault {
		t.Errorf("the SPIRE Service is at %q, but CWE's SPIRE_SERVER_API_ADDRESS defaults to %q; "+
			"join-token operations would dial a port nothing serves", got, cweAPIAddressDefault)
	}

	args := joinArgs(spireInstallArgs(Config{}, "/tmp/values.yaml", "kind", "kind.nke.local", time.Minute))
	if !strings.Contains(args, "--set "+spireServerValuesPrefix+".service.port="+spireServerPort) {
		t.Errorf("SPIRE install does not move the Service off the chart's default port: %s", args)
	}

	// The agent's port is its own value in the chart and does not follow the
	// Service. Moving one without the other leaves the agent dialling a port
	// the Service does not publish, which a ClusterIP blackholes rather than
	// refuses -- so it fails as an attestation timeout naming no port at all.
	if !strings.Contains(args, "--set "+spireAgentValuesPrefix+".server.port="+spireServerPort) {
		t.Errorf("SPIRE install moves the Service but not the agent, so the agent would "+
			"dial a port nothing publishes: %s", args)
	}
}
