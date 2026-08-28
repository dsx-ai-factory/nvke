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
}

type fakeOutput struct {
	output string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	idx := len(f.calls) - 1
	if idx < len(f.outputs) {
		return f.outputs[idx].output, f.outputs[idx].err
	}
	if len(f.outputs) > 0 {
		last := f.outputs[len(f.outputs)-1]
		return last.output, last.err
	}
	return "", nil
}

func joinArgs(args []string) string { return strings.Join(args, " ") }

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

func TestDeploy_InstallsPostgresTemporalThenCWEInOrder(t *testing.T) {
	runner := &fakeRunner{}
	e := New(runner, nil)

	cfg := Config{
		CWEImage:         "nvcr.io/org/controller:edge",
		ProfilesImage:    "nvcr.io/org/cwe-profiles:v1",
		ChartPath:        t.TempDir(), // any existing dir stands in for the chart
		PostgresPassword: "test-password",
	}
	if err := e.Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 helm calls (postgres, temporal, cwe), got %d: %+v", len(runner.calls), runner.calls)
	}

	for i, call := range runner.calls {
		if call.name != "helm" {
			t.Errorf("call[%d].name = %q, want %q", i, call.name, "helm")
		}
		if joinArgs(call.args) == "" || call.args[0] != "--kubeconfig" || call.args[1] != "/tmp/kubeconfig" {
			t.Errorf("call[%d] missing --kubeconfig /tmp/kubeconfig: %s", i, joinArgs(call.args))
		}
	}

	postgres, temporal, cwe := joinArgs(runner.calls[0].args), joinArgs(runner.calls[1].args), joinArgs(runner.calls[2].args)

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
}

func TestDeploy_PropagatesHelmError(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		{output: "Error: INSTALLATION FAILED", err: errors.New("exit status 1")},
	}}
	e := New(runner, nil)

	err := e.Deploy(context.Background(), "/tmp/kubeconfig", Config{
		CWEImage:      "nvcr.io/org/controller:edge",
		ProfilesImage: "nvcr.io/org/cwe-profiles:v1",
		ChartPath:     t.TempDir(),
	})
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
	cfg := Config{
		CWEImage:         "ghcr.io/nvidia/nke/controller@" + digest,
		ProfilesImage:    "nvcr.io/org/cwe-profiles:v1",
		ChartPath:        t.TempDir(),
		PostgresPassword: "test-password",
	}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	cwe := joinArgs(runner.calls[2].args)
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
