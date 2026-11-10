// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
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

	// chartFiles holds, per release, the files present in the chart directory
	// that release's helm install was pointed at. See captureChart.
	chartFiles map[string]map[string]bool

	// valuesFiles holds, per release, the contents of the first --values file
	// its helm install was pointed at. extraValuesFiles holds any that follow.
	// See captureValues.
	valuesFiles      map[string]string
	extraValuesFiles map[string][]string
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

// captureChart records the files in the chart directory a helm install was
// pointed at, for the same reason captureApply exists: an extracted chart is
// removed as soon as Deploy returns, so a test that looked afterwards would
// find nothing.
func (f *fakeRunner) captureChart(name string, args []string) {
	if name != "helm" || len(args) == 0 {
		return
	}
	joined := joinArgs(args)
	// The two releases installed from a chart carried in the binary. The rest
	// name an upstream chart, which has no directory to walk.
	for _, release := range []string{cweReleaseName, postgresReleaseName} {
		if !strings.Contains(joined, "upgrade --install "+release+" ") {
			continue
		}
		fields := strings.Fields(joined)
		for i, field := range fields {
			if field != release || i+1 >= len(fields) {
				continue
			}
			root := fields[i+1]
			files := map[string]bool{}
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil //nolint:nilerr // a missing chart is what the test asserts on
				}
				rel, relErr := filepath.Rel(root, path)
				if relErr == nil {
					files[filepath.ToSlash(rel)] = true
				}
				return nil
			})
			if f.chartFiles == nil {
				f.chartFiles = map[string]map[string]bool{}
			}
			f.chartFiles[release] = files
			return
		}
	}
}

// captureValues records the contents of the --values file each helm install
// was pointed at, for the same reason captureApply exists: Deploy removes the
// file as soon as the install returns.
func (f *fakeRunner) captureValues(name string, args []string) {
	if name != "helm" {
		return
	}
	release := ""
	for i, arg := range args {
		if arg == "--install" && i+1 < len(args) {
			release = args[i+1]
		}
	}
	if release == "" {
		return
	}
	for i, arg := range args {
		if arg != "--values" || i+1 >= len(args) {
			continue
		}
		body, err := os.ReadFile(args[i+1])
		if err != nil {
			return
		}
		if f.valuesFiles == nil {
			f.valuesFiles = map[string]string{}
		}
		if _, ok := f.valuesFiles[release]; !ok {
			f.valuesFiles[release] = string(body)
			continue
		}
		if f.extraValuesFiles == nil {
			f.extraValuesFiles = map[string][]string{}
		}
		f.extraValuesFiles[release] = append(f.extraValuesFiles[release], string(body))
	}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	f.captureApply(name, args)
	f.captureChart(name, args)
	f.captureValues(name, args)
	idx := len(f.calls) - 1
	if idx < len(f.outputs) {
		return f.outputs[idx].output, f.outputs[idx].err
	}
	if len(f.outputs) > 0 {
		last := f.outputs[len(f.outputs)-1]
		return last.output, last.err
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

// testConfig is a Config with every required field filled in, so a test that
// cares about one of them can set just that one.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		CWEImage:                 "nvcr.io/org/controller:edge",
		ProvisionedProviderImage: "cluster-api-provider-provisioned:latest",
		PostgresPassword:         "test-password",
	}
}

func TestDefaultConfig_CWEPort(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_PORT", "30181")

	cfg := DefaultConfig()
	if cfg.CWEPort != 30181 {
		t.Errorf("DefaultConfig port = %d, want 30181", cfg.CWEPort)
	}
}

func TestTenantKubeconfigRetriesUntilPublished(t *testing.T) {
	oldInterval := tenantKubeconfigPollInterval
	tenantKubeconfigPollInterval = time.Millisecond
	t.Cleanup(func() { tenantKubeconfigPollInterval = oldInterval })

	runner := &fakeRunner{outputs: []fakeOutput{
		{output: "Error from server (NotFound)", err: errors.New("exit status 1")},
		{output: "temporary API failure", err: errors.New("exit status 1")},
		{output: base64.StdEncoding.EncodeToString([]byte("apiVersion: v1\nkind: Config\n"))},
	}}
	got, err := New(runner, nil).TenantKubeconfig(
		context.Background(), "/tmp/kubeconfig", "nvke-site", time.Second,
	)
	if err != nil {
		t.Fatalf("TenantKubeconfig returned error: %v", err)
	}
	if !strings.Contains(string(got), "kind: Config") {
		t.Errorf("TenantKubeconfig = %q, want a kubeconfig", got)
	}
	if len(runner.calls) != 3 {
		t.Errorf("TenantKubeconfig made %d attempts, want 3", len(runner.calls))
	}
}

func TestTenantKubeconfigTimesOutWithLastError(t *testing.T) {
	oldInterval := tenantKubeconfigPollInterval
	tenantKubeconfigPollInterval = time.Millisecond
	t.Cleanup(func() { tenantKubeconfigPollInterval = oldInterval })

	runner := &fakeRunner{outputs: []fakeOutput{{
		output: "Error from server (NotFound): secrets \"nke-site-kubeconfig\" not found",
		err:    errors.New("exit status 1"),
	}}}
	_, err := New(runner, nil).TenantKubeconfig(
		context.Background(), "/tmp/kubeconfig", "nvke-site", 5*time.Millisecond,
	)
	if err == nil {
		t.Fatal("TenantKubeconfig did not time out")
	}
	for _, want := range []string{"not published within", "NotFound"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
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
	wantProxyPath := "/api/v1/namespaces/nke-controller/services/nke-controller:http/proxy/readyz"
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

// TestDumpDiagnostics covers the failure-path bundle: it must collect the
// cluster dump plus the extras that explain a pod which never started, and it
// must not turn a diagnostics problem into the error the caller sees.
func TestDumpDiagnostics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
	runner := &fakeRunner{outputs: []fakeOutput{{output: "collected"}}}

	New(runner, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	// The dump goes into a private directory outside dir, so nothing unscrubbed
	// is ever written to the directory that gets shared.
	var dumpedTo string
	for _, call := range runner.calls {
		if !strings.Contains(joinArgs(call.args), "cluster-info dump") {
			continue
		}
		for i, arg := range call.args {
			if arg == "--output-directory" && i+1 < len(call.args) {
				dumpedTo = call.args[i+1]
			}
		}
	}
	if dumpedTo == "" {
		t.Fatalf("no cluster-info dump with an output directory; calls were %+v", runner.calls)
	}
	if dumpedTo == dir || strings.HasPrefix(dumpedTo, dir+string(filepath.Separator)) {
		t.Errorf("the dump went into %s, which is inside %s", dumpedTo, dir)
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

func TestSpireTPMDirectChecksum(t *testing.T) {
	tests := map[string]string{
		"amd64": spireTPMDirectAMD64Checksum,
		"arm64": spireTPMDirectARM64Checksum,
	}
	for arch, want := range tests {
		if got := SpireTPMDirectChecksum(arch); got != want {
			t.Errorf("SpireTPMDirectChecksum(%s) = %q, want %q", arch, got, want)
		}
	}
	for _, arch := range []string{"ppc64le", "s390x"} {
		if got := SpireTPMDirectChecksum(arch); got != "" {
			t.Errorf("SpireTPMDirectChecksum(%s) = %q, want no checksum", arch, got)
		}
	}
}

// TestDefaultConfig_ImagesDefaultToTheManifest: with neither variable set the
// two built images are the manifest's refs; a set variable wins.
func TestDefaultConfig_ImagesDefaultToTheManifest(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "")
	cfg := DefaultConfig()
	m := manifest.Load()
	if want := m.Components[manifest.ComponentController].Images[manifest.ImageController].Ref; cfg.CWEImage != want || want == "" {
		t.Errorf("CWEImage = %q, want the manifest's %q", cfg.CWEImage, want)
	}
	if want := m.Components[manifest.ComponentProvisioned].Images[manifest.ImageManager].Ref; cfg.ProvisionedProviderImage != want || want == "" {
		t.Errorf("ProvisionedProviderImage = %q, want the manifest's %q", cfg.ProvisionedProviderImage, want)
	}

	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "nvcr.io/org/nvke-controller:edge")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "nvcr.io/org/provisioned:edge")
	cfg = DefaultConfig()
	if cfg.CWEImage != "nvcr.io/org/nvke-controller:edge" || cfg.ProvisionedProviderImage != "nvcr.io/org/provisioned:edge" {
		t.Errorf("environment did not win: %+v", cfg)
	}
}

// renamedSettingCases is every setting this package reads, with its previous
// NKECTL_* name (none for an NVKECTL_NVKE_* setting), two values it accepts and
// the Config field that carries it.
var renamedSettingCases = []struct {
	name, old  string
	oldV, newV string
	got        func(Config) string
}{
	{"NVKECTL_NVKE_CONTROLLER_IMAGE", "", "nvcr.io/org/old:1", "nvcr.io/org/new:1", func(c Config) string { return c.CWEImage }},
	{"NVKECTL_NVKE_CONTROLLER_PORT", "", "30181", "30182", func(c Config) string { return strconv.Itoa(int(c.CWEPort)) }},
	{"NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT", "", "1m0s", "2m0s", func(c Config) string { return c.HelmTimeout.String() }},
	{"NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT", "", "3m0s", "4m0s", func(c Config) string { return c.ReadyTimeout.String() }},
	{"NVKECTL_CAPI_PROVISIONED_IMAGE", "NKECTL_CAPI_PROVISIONED_IMAGE", "nvcr.io/org/old-provisioned:1", "nvcr.io/org/new-provisioned:1", func(c Config) string { return c.ProvisionedProviderImage }},
	{"NVKECTL_SPIFFE_TRUST_DOMAIN", "NKECTL_SPIFFE_TRUST_DOMAIN", "old.example", "new.example", func(c Config) string { return c.SPIFFETrustDomain }},
}

// clearSettings blanks every setting this package reads, under every name it
// has had.
func clearSettings(t *testing.T) {
	t.Helper()
	for _, c := range renamedSettingCases {
		t.Setenv(c.name, "")
		if c.old != "" {
			t.Setenv(c.old, "")
		}
	}
}

// TestDefaultConfig_RenamedSettings: each setting with a previous NKECTL_*
// name is read; that name is read when the new one is unset and is then
// reported as deprecated; with both set the new one wins and nothing is
// reported.
func TestDefaultConfig_RenamedSettings(t *testing.T) {
	if len(renamedSettingCases) != len(Settings) {
		t.Fatalf("%d cases for %d settings", len(renamedSettingCases), len(Settings))
	}
	for _, tc := range renamedSettingCases {
		if tc.old == "" {
			continue
		}
		for _, set := range []struct {
			desc             string
			newV, oldV, want string
			deprecated       bool
		}{
			{"new name only", tc.newV, "", tc.newV, false},
			{"old name only", "", tc.oldV, tc.oldV, true},
			{"both names", tc.newV, tc.oldV, tc.newV, false},
		} {
			t.Run(tc.name+"/"+set.desc, func(t *testing.T) {
				clearSettings(t)
				t.Setenv(tc.name, set.newV)
				t.Setenv(tc.old, set.oldV)

				if got := tc.got(DefaultConfig()); got != set.want {
					t.Errorf("DefaultConfig read %q, want %q", got, set.want)
				}
				var want []envalias.Renamed
				if set.deprecated {
					want = []envalias.Renamed{{Old: tc.old, New: tc.name}}
				}
				if got := DeprecatedSettings(); !slices.Equal(got, want) {
					t.Errorf("DeprecatedSettings = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestDefaultConfig_ControllerSettings: each NVKECTL_NVKE_CONTROLLER_* setting
// is read under that name only. An earlier name (the cli/v0.2.0 release
// candidates' NVKECTL_CONTROLLER_* and NKECTL_CONTROLLER_*, or cli/v0.1.x's
// NKECTL_CWE_*) is neither read nor reported as deprecated, alone or beside the
// new one.
func TestDefaultConfig_ControllerSettings(t *testing.T) {
	for _, tc := range renamedSettingCases {
		suffix, ok := strings.CutPrefix(tc.name, "NVKECTL_NVKE_CONTROLLER_")
		if !ok {
			continue
		}
		for _, prefix := range []string{"NVKECTL_CONTROLLER_", "NKECTL_CONTROLLER_", "NKECTL_CWE_"} {
			old := prefix + suffix
			t.Run(tc.name+"/"+old, func(t *testing.T) {
				clearSettings(t)
				t.Setenv(old, tc.oldV)
				if got := tc.got(DefaultConfig()); got == tc.oldV {
					t.Errorf("DefaultConfig read %s", old)
				}

				t.Setenv(tc.name, tc.newV)
				if got := tc.got(DefaultConfig()); got != tc.newV {
					t.Errorf("DefaultConfig read %q, want %q", got, tc.newV)
				}
				if got := DeprecatedSettings(); len(got) != 0 {
					t.Errorf("DeprecatedSettings = %v, want none", got)
				}
			})
		}
	}
}

// TestDeprecatedSettings_SortedByOldName: several old names at once come back
// in a stable order, so the warnings print the same way every run.
func TestDeprecatedSettings_SortedByOldName(t *testing.T) {
	clearSettings(t)
	t.Setenv("NKECTL_SPIFFE_TRUST_DOMAIN", "old.example")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "nvcr.io/org/old-provisioned:1")

	want := []envalias.Renamed{
		{Old: "NKECTL_CAPI_PROVISIONED_IMAGE", New: "NVKECTL_CAPI_PROVISIONED_IMAGE"},
		{Old: "NKECTL_SPIFFE_TRUST_DOMAIN", New: "NVKECTL_SPIFFE_TRUST_DOMAIN"},
	}
	if got := DeprecatedSettings(); !slices.Equal(got, want) {
		t.Errorf("DeprecatedSettings = %v, want %v", got, want)
	}
}

func TestHelmRetriesChartDownloadServerErrors(t *testing.T) {
	oldInterval := helmFetchRetryInterval
	helmFetchRetryInterval = time.Millisecond
	t.Cleanup(func() { helmFetchRetryInterval = oldInterval })

	exit1 := errors.New("exit status 1")
	fetch500 := "Error: failed to fetch https://github.com/spiffe/helm-charts-hardened/releases/download/spire-crds-0.6.1/spire-crds-0.6.1.tgz : 500 Internal Server Error"
	cases := []struct {
		name      string
		outputs   []fakeOutput
		wantCalls int
		wantErr   bool
	}{
		{"a 500 on the download is retried", []fakeOutput{{output: fetch500, err: exit1}, {}}, 2, false},
		{"a 500 on every attempt gives up after three", []fakeOutput{{output: fetch500, err: exit1}}, 3, true},
		{"any other failure is not retried", []fakeOutput{{output: "Error: INSTALLATION FAILED: timed out waiting for the condition", err: exit1}}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{outputs: tc.outputs}
			_, err := New(runner, nil).helm(context.Background(), "/tmp/kubeconfig",
				"upgrade", "--install", spireCRDsReleaseName, "spire-crds", "--repo", "https://spiffe.github.io/helm-charts-hardened/")
			if (err != nil) != tc.wantErr {
				t.Fatalf("helm returned error %v, want error %v", err, tc.wantErr)
			}
			if len(runner.calls) != tc.wantCalls {
				t.Errorf("helm made %d attempts, want %d", len(runner.calls), tc.wantCalls)
			}
		})
	}
}

// fakeValue stands in for the generated password. It is deliberately plain.
const fakeValue = "aaaa-bbbb-cccc"

// failingHelmRunner fails the failAt-th helm call, with output that echoes the
// password as helm can, and records the rest.
type failingHelmRunner struct {
	*fakeRunner
	failAt int
	helms  int
}

func (r *failingHelmRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	if name != "helm" {
		return out, err
	}
	r.helms++
	if r.helms-1 == r.failAt {
		return "Error: INSTALLATION FAILED: " + fakeValue + " rejected", errors.New("exit status 1")
	}
	return out, err
}

// dumpingRunner makes `kubectl cluster-info dump` write files that hold a
// credential, as the controller's pod spec would, so a test can check that
// DumpDiagnostics scrubs what the cluster gave it.
type dumpingRunner struct {
	*fakeRunner
}

func (r *dumpingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	for i, a := range args {
		if a != "--output-directory" || i+1 >= len(args) {
			continue
		}
		sub := filepath.Join(args[i+1], "nke-controller")
		if mkErr := os.MkdirAll(sub, 0o750); mkErr != nil {
			return out, mkErr
		}
		pod := `{"env":[{"name":"DATABASE_URL","value":"postgres://u:` + fakeValue + `@h/db"}],"keep":"host=db"}`
		other := `{"env":[{"name":"DATABASE_URL","value":"postgres://u:not-registered@h/db"}]}`
		for file, body := range map[string]string{"pods.json": pod, "other.json": other} {
			if wErr := os.WriteFile(filepath.Join(sub, file), []byte(body), 0o600); wErr != nil {
				return out, wErr
			}
		}
	}
	return out, err
}

// TestDumpDiagnostics_ScrubsCredentials guards the scrub that runs after the
// dump. The files CI uploads are what it protects: with the call removed, every
// other test still passes.
func TestDumpDiagnostics_ScrubsCredentials(t *testing.T) {
	redact.RegisterSecret(fakeValue)
	dir := filepath.Join(t.TempDir(), "cluster-diagnostics")

	New(&dumpingRunner{fakeRunner: &fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	for file, mustNotHold := range map[string]string{
		"pods.json":  fakeValue,        // a registered password, in a URL
		"other.json": "not-registered", // any password in a database URL
	} {
		body, err := os.ReadFile(filepath.Join(dir, "nke-controller", file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if strings.Contains(string(body), mustNotHold) {
			t.Errorf("%s still holds %q after the dump:\n%s", file, mustNotHold, body)
		}
		if !strings.Contains(string(body), "@h/db") {
			t.Errorf("%s lost more than the password:\n%s", file, body)
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, "nke-controller", "pods.json"))
	if err != nil || !strings.Contains(string(body), `"keep":"host=db"`) {
		t.Errorf("an unrelated field did not survive: %q, %v", body, err)
	}
}

// The credentials are in values files, so no argument should carry one. If a
// future change puts one back, in any argument shape, the command an error prints
// must still not show it.
func TestHelm_MasksARegisteredCredentialInAnArgument(t *testing.T) {
	redact.RegisterSecret(fakeValue)
	runner := &failingHelmRunner{fakeRunner: &fakeRunner{}, failAt: 0}

	_, err := New(runner, nil).helm(context.Background(), "/tmp/kubeconfig",
		"upgrade", "--install", "demo", "chart", "--set", "a=1,db.pw="+fakeValue, "--set-json=x="+fakeValue)
	if err == nil {
		t.Fatal("expected helm to fail")
	}
	if strings.Contains(err.Error(), fakeValue) {
		t.Errorf("the error holds the credential:\n%s", err)
	}
	if !strings.Contains(err.Error(), "--install demo chart") {
		t.Errorf("the rest of the command should stay readable:\n%s", err)
	}
}

// --diagnostics-dir can be any directory, including one that holds the caller's
// own files. The scrub must leave those alone: it rewrites every file it visits.
func TestDumpDiagnostics_LeavesOtherFilesInTheDirectoryAlone(t *testing.T) {
	redact.RegisterSecret(fakeValue)
	dir := t.TempDir()
	others := map[string]string{
		"app.env":          "DATABASE_URL=postgres://u:p@h/db\n",
		"old.log":          "rejected " + fakeValue + "\n",
		"sub/notes.txt":    "postgresql://dev:hunter2@localhost/app\n",
		"nke-controller/x": "a file in a directory the bundle also uses\n",
	}
	for name, body := range others {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(dir, "all-resources.txt")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	New(&dumpingRunner{fakeRunner: &fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	for name, want := range others {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want it untouched: %q", name, got, err, want)
		}
	}
	if got, _ := os.ReadFile(stale); string(got) == "stale" {
		t.Error("a bundle file did not replace the file of the same name")
	}
	body, err := os.ReadFile(filepath.Join(dir, "nke-controller", "pods.json"))
	if err != nil {
		t.Fatalf("the bundle was not published into dir: %v", err)
	}
	if strings.Contains(string(body), fakeValue) {
		t.Errorf("a published bundle file still holds the credential: %s", body)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nvkectl-dump-") {
			t.Errorf("the staging directory %s was left behind", e.Name())
		}
	}
}

// A directory the caller cannot read is theirs, not part of the bundle, so it is
// not a reason for the scrub to complain or for the bundle to go missing.
func TestDumpDiagnostics_IgnoresAnUnreadableDirectoryAlreadyThere(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not stop it")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	New(&fakeRunner{}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	if _, err := os.Stat(filepath.Join(dir, "helm-releases.txt")); err != nil {
		t.Errorf("the bundle was not published: %v", err)
	}
}

// readOnlyRunner makes `kubectl cluster-info dump` write a file that holds the
// credential and cannot be rewritten, next to one that can.
type readOnlyRunner struct{ *fakeRunner }

func (r readOnlyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	for i, a := range args {
		if a != "--output-directory" || i+1 >= len(args) {
			continue
		}
		body := `{"u":"postgres://x:` + fakeValue + `@h/db"}`
		for file, mode := range map[string]os.FileMode{"locked.json": 0o400, "fine.json": 0o600} {
			if wErr := os.WriteFile(filepath.Join(args[i+1], file), []byte(body), mode); wErr != nil {
				return out, wErr
			}
		}
	}
	return out, err
}

// A file the scrub could not rewrite still holds the credential, so it must not
// reach the directory that gets shared.
func TestDumpDiagnostics_WithholdsAFileItCouldNotScrub(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not stop it")
	}
	redact.RegisterSecret(fakeValue)
	dir := t.TempDir()

	New(readOnlyRunner{&fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	if _, err := os.Stat(filepath.Join(dir, "locked.json")); err == nil {
		t.Error("a file that could not be scrubbed was published")
	}
	got, err := os.ReadFile(filepath.Join(dir, "fine.json"))
	if err != nil {
		t.Fatalf("a scrubbed file was not published: %v", err)
	}
	if strings.Contains(string(got), fakeValue) {
		t.Errorf("a published file still holds the credential: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "helm-releases.txt")); err != nil {
		t.Errorf("the rest of the bundle was not published: %v", err)
	}
	var leaked []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), fakeValue) {
				leaked = append(leaked, path)
			}
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("the credential reached %v", leaked)
	}
}

// lockedDirRunner makes `kubectl cluster-info dump` write a non-empty directory
// that cannot be read, which blocks removing the staging directory and which the
// scrub cannot look inside.
type lockedDirRunner struct{ *fakeRunner }

func (r lockedDirRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	for i, a := range args {
		if a != "--output-directory" || i+1 >= len(args) {
			continue
		}
		locked := filepath.Join(args[i+1], "locked")
		if mkErr := os.MkdirAll(locked, 0o700); mkErr != nil {
			return out, mkErr
		}
		body := `{"u":"postgres://x:` + fakeValue + `@h/db"}`
		if wErr := os.WriteFile(filepath.Join(locked, "raw.json"), []byte(body), 0o600); wErr != nil {
			return out, wErr
		}
		if cErr := os.Chmod(locked, 0); cErr != nil {
			return out, cErr
		}
	}
	return out, err
}

// The dump holds credentials until it is scrubbed, so it must never be left
// where it gets shared. A directory the cluster produced that cannot be read
// must neither be published nor stop the staging directory being deleted.
func TestDumpDiagnostics_LeavesNoRawDumpBehind(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not stop it")
	}
	redact.RegisterSecret(fakeValue)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := t.TempDir()

	New(lockedDirRunner{&fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nvkectl-") || e.Name() == "locked" {
			t.Errorf("%s was left in the shared directory", e.Name())
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the staging directory was not removed: %v", left)
	}
	if _, err := os.Stat(filepath.Join(dir, "helm-releases.txt")); err != nil {
		t.Errorf("the rest of the bundle was not published: %v", err)
	}
}

// If the staging directory cannot be deleted, the caller has to be told where it
// is, since it holds the dump as the cluster produced it.
func TestRemoveStaging_NamesADirectoryItCannotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not stop it")
	}
	parent := t.TempDir()
	staging := filepath.Join(parent, "staging")
	if err := os.MkdirAll(filepath.Join(staging, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory that cannot be written to cannot have its entries removed.
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	var logs bytes.Buffer

	New(&fakeRunner{}, slog.New(slog.NewTextHandler(&logs, nil))).removeStaging(staging)

	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), staging) {
		t.Errorf("the failure to remove %s was not reported with its path:\n%s", staging, logs.String())
	}
}

// modeRunner makes `kubectl cluster-info dump` write a file with a mode that no
// default would give it.
type modeRunner struct{ *fakeRunner }

func (r modeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	for i, a := range args {
		if a != "--output-directory" || i+1 >= len(args) {
			continue
		}
		path := filepath.Join(args[i+1], "plain.txt")
		if wErr := os.WriteFile(path, []byte("nothing secret"), 0o600); wErr != nil {
			return out, wErr
		}
		if cErr := os.Chmod(path, 0o640); cErr != nil {
			return out, cErr
		}
	}
	return out, err
}

// A published file keeps the mode the dump gave it, and no temporary copy is left
// next to it.
func TestDumpDiagnostics_PublishesFilesWithTheirModes(t *testing.T) {
	dir := t.TempDir()

	New(modeRunner{&fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	info, err := os.Stat(filepath.Join(dir, "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("plain.txt mode = %o, want 640", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nvkectl-") {
			t.Errorf("%s was left behind", e.Name())
		}
	}
}

// symlinkRunner makes `kubectl cluster-info dump` write a symlink to a file
// outside the dump.
type symlinkRunner struct {
	*fakeRunner
	target string
}

func (r symlinkRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	for i, a := range args {
		if a == "--output-directory" && i+1 < len(args) {
			if lErr := os.Symlink(r.target, filepath.Join(args[i+1], "link.json")); lErr != nil {
				return out, lErr
			}
		}
	}
	return out, err
}

// The scrub does not look at a symlink, and copying one would publish whatever
// it points at, so a symlink in the dump must not reach the directory.
func TestDumpDiagnostics_DoesNotFollowASymlinkInTheDump(t *testing.T) {
	redact.RegisterSecret(fakeValue)
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(outside, []byte("a file elsewhere on the host: "+fakeValue), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	New(symlinkRunner{&fakeRunner{}, outside}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

	if _, err := os.Lstat(filepath.Join(dir, "link.json")); err == nil {
		t.Error("a symlink in the dump was published")
	}
	if _, err := os.Stat(filepath.Join(dir, "helm-releases.txt")); err != nil {
		t.Errorf("the rest of the bundle was not published: %v", err)
	}
}

// A symlink already in the diagnostics directory, where the bundle wants a
// directory, must not carry the bundle to somewhere else: files outside the
// directory are not ours to replace or add to.
func TestDumpDiagnostics_DoesNotWriteThroughASymlinkInTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, dir, elsewhere string)
	}{
		{"a symlink to a directory", func(t *testing.T, dir, elsewhere string) {
			if err := os.Symlink(elsewhere, filepath.Join(dir, "nke-controller")); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
		}},
		{"a file where a directory is wanted", func(t *testing.T, dir, _ string) {
			if err := os.WriteFile(filepath.Join(dir, "nke-controller"), []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elsewhere := t.TempDir()
			unrelated := filepath.Join(elsewhere, "pods.json")
			const before = "an unrelated file that must not change"
			if err := os.WriteFile(unrelated, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			tc.make(t, dir, elsewhere)

			New(&dumpingRunner{fakeRunner: &fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

			if got, _ := os.ReadFile(unrelated); string(got) != before {
				t.Errorf("a file outside the diagnostics directory was changed: %q", got)
			}
			if entries, _ := os.ReadDir(elsewhere); len(entries) != 1 {
				t.Errorf("files were written outside the diagnostics directory: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(dir, "helm-releases.txt")); err != nil {
				t.Errorf("the rest of the bundle was not published: %v", err)
			}
		})
	}
}

// --diagnostics-dir is the caller's own, so it may itself be a symlink: only what
// is under it is not trusted.
func TestDumpDiagnostics_PublishesIntoADirectoryThatIsASymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	New(&dumpingRunner{fakeRunner: &fakeRunner{}}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", link)

	for _, name := range []string{"helm-releases.txt", filepath.Join("nke-controller", "pods.json")} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("%s was not published into the directory the symlink points at: %v", name, err)
		}
	}
}

func TestDestination(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rel     string
		setup   func(root, elsewhere string)
		wantErr bool
		// needsPerms skips the case as root, which permissions do not stop.
		needsPerms bool
	}{
		{name: "a top-level file", rel: "a.txt"},
		{name: "nested, nothing there yet", rel: "ns/pod/logs.txt"},
		{
			name: "nested, the directories are already there",
			rel:  "ns/pod/logs.txt",
			setup: func(r, _ string) {
				_ = os.MkdirAll(filepath.Join(r, "ns", "pod"), 0o750)
			},
		},
		{
			name:    "a directory on the way is a symlink to a directory",
			rel:     "ns/logs.txt",
			wantErr: true,
			setup: func(r, e string) {
				_ = os.Symlink(e, filepath.Join(r, "ns"))
			},
		},
		{
			name:    "a deeper directory is a symlink",
			rel:     "ns/pod/logs.txt",
			wantErr: true,
			setup: func(r, e string) {
				_ = os.MkdirAll(filepath.Join(r, "ns"), 0o750)
				_ = os.Symlink(e, filepath.Join(r, "ns", "pod"))
			},
		},
		{
			name:    "a directory on the way is a dangling symlink",
			rel:     "ns/logs.txt",
			wantErr: true,
			setup: func(r, _ string) {
				_ = os.Symlink(filepath.Join(r, "missing"), filepath.Join(r, "ns"))
			},
		},
		{
			name:    "a directory on the way is a regular file",
			rel:     "ns/logs.txt",
			wantErr: true,
			setup: func(r, _ string) {
				_ = os.WriteFile(filepath.Join(r, "ns"), []byte("x"), 0o600)
			},
		},
		{
			name:       "a directory on the way cannot be entered",
			rel:        "ns/pod/logs.txt",
			wantErr:    true,
			needsPerms: true,
			setup: func(r, _ string) {
				_ = os.Mkdir(filepath.Join(r, "ns"), 0o750)
				_ = os.Chmod(filepath.Join(r, "ns"), 0)
			},
		},
		{
			name:       "the root cannot be written to",
			rel:        "ns/logs.txt",
			wantErr:    true,
			needsPerms: true,
			setup: func(r, _ string) {
				_ = os.Chmod(r, 0o500)
			},
		},
		{name: "a name too long for the filesystem", rel: strings.Repeat("a", 300) + "/logs.txt", wantErr: true},
		{name: "a path that climbs out", rel: "../escaped/logs.txt", wantErr: true},
		{name: "a path that climbs out after a directory", rel: "ns/../../escaped.txt", wantErr: true},
		{name: "an absolute path", rel: "/etc/passwd", wantErr: true},
		{name: "an empty path", rel: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsPerms && os.Geteuid() == 0 {
				t.Skip("running as root: file permissions do not stop it")
			}
			parent := t.TempDir()
			root, elsewhere := filepath.Join(parent, "root"), filepath.Join(parent, "elsewhere")
			for _, d := range []string{root, elsewhere} {
				if err := os.Mkdir(d, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setup != nil {
				tc.setup(root, elsewhere)
			}
			t.Cleanup(func() {
				_ = filepath.WalkDir(parent, func(p string, _ fs.DirEntry, _ error) error { _ = os.Chmod(p, 0o700); return nil })
			})

			dst, err := destination(root, tc.rel)

			if (err != nil) != tc.wantErr {
				t.Fatalf("destination(%q) error = %v, want an error: %v", tc.rel, err, tc.wantErr)
			}
			if err == nil && !strings.HasPrefix(dst, root+string(filepath.Separator)) {
				t.Errorf("destination(%q) = %s, which is not inside %s", tc.rel, dst, root)
			}
			if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
				t.Errorf("something was created outside the root: %v", entries)
			}
		})
	}
}

// Two processes can publish into the same directory at once. Finding a directory
// that someone else just made must not count as a failure.
func TestDestination_ConcurrentWritersSharingDirectories(t *testing.T) {
	for round := 0; round < 100; round++ {
		root := t.TempDir()
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		start := make(chan struct{})
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := destination(root, filepath.Join("ns", "pod", "logs.txt")); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

// The name a bundle file is published under can already hold something else.
// A directory is not the caller's to lose, so it stays and the file is skipped;
// a symlink is replaced as a name, and what it pointed at is left alone.
func TestDumpDiagnostics_WhatAlreadyHasTheFinalName(t *testing.T) {
	t.Run("a directory is left as it was", func(t *testing.T) {
		dir := t.TempDir()
		keep := filepath.Join(dir, "all-resources.txt", "mine", "keep.txt")
		if err := os.MkdirAll(filepath.Dir(keep), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}

		New(&fakeRunner{}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

		if got, err := os.ReadFile(keep); err != nil || string(got) != "keep" {
			t.Errorf("the caller's file under the directory = %q, %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "helm-releases.txt")); err != nil {
			t.Errorf("the rest of the bundle was not published: %v", err)
		}
		if stray, _ := filepath.Glob(filepath.Join(dir, ".nvkectl-copy-*")); len(stray) != 0 {
			t.Errorf("temporary copies were left behind: %v", stray)
		}
	})

	t.Run("a symlink is replaced and its target is left alone", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target.txt")
		if err := os.WriteFile(target, []byte("target content"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "helm-releases.txt")); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}

		New(&fakeRunner{}, nil).DumpDiagnostics(context.Background(), "/tmp/kubeconfig", dir)

		if got, _ := os.ReadFile(target); string(got) != "target content" {
			t.Errorf("the file the symlink pointed at was changed: %q", got)
		}
		if info, err := os.Lstat(filepath.Join(dir, "helm-releases.txt")); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the name should now hold the published file: %v, %v", info, err)
		}
	})
}

func TestValidateProfilesImage(t *testing.T) {
	tests := []struct {
		image   string
		wantErr string
	}{
		{"", ""},
		{"controller-profiles:e2e", ""},
		{"localhost:5000/team/profiles:v1", ""},
		{"registry.example.com/profiles@sha256:abc123", ""},
		{"controller-profiles:latest", "other than latest"},
		{"controller-profiles", "other than latest"},
		{"localhost:5000/team/profiles", "other than latest"},
		{"registry.example.com/profiles:", "empty tag"},
		{"localhost:5000/team/profiles:", "empty tag"},
		{"registry.example.com/profiles:@sha256:abc123", "empty tag"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			err := ValidateProfilesImage(tt.image)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ValidateProfilesImage(%q) = %v, want nil", tt.image, err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), ProfilesImageEnv)):
				t.Fatalf("ValidateProfilesImage(%q) = %v, want one naming %s and %q", tt.image, err, ProfilesImageEnv, tt.wantErr)
			}
		})
	}
}
