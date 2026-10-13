// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
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
		ProvisionedProviderImage: "cluster-api-provider-provisioned:latest",
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
	if err := e.Deploy(context.Background(), "/tmp/kubeconfig", Config{
		CWEImage: "nvcr.io/org/controller:edge",
	}); err == nil {
		t.Fatal("expected an error for a missing Config.ProvisionedProviderImage")
	}
}

func TestCWEEnvArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want []string
	}{{
		name: "nothing is sent when nothing is configured",
		cfg:  Config{},
		want: nil,
	}, {
		name: "the node agent port is sent as the first env entry",
		cfg:  Config{NodeAgentPort: "15005"},
		want: []string{
			"--set-string", "env[0].name=NODE_AGENT_PORT",
			"--set-string", "env[0].value=15005",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cweEnvArgs(tc.cfg); !slices.Equal(got, tc.want) {
				t.Errorf("cweEnvArgs =\n\t%q\nwant\n\t%q", got, tc.want)
			}
		})
	}
}

func TestDefaultConfig_CWEPort(t *testing.T) {
	t.Setenv("NVKECTL_CONTROLLER_PORT", "30181")

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

func TestDeploy_PublishesCWEAfterTheRelease(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	cwe := callIndex(runner, "upgrade --install "+cweReleaseName)
	nodePort := -1
	for i, call := range runner.calls {
		args := joinArgs(call.args)
		if call.name == "kubectl" && strings.Contains(args, "apply") && !strings.Contains(args, "--server-side") {
			nodePort = i
		}
	}
	if nodePort < cwe {
		t.Errorf("the controller NodePort (%d) was applied before the controller itself (%d)", nodePort, cwe)
	}
	if want := fmt.Sprintf("nodePort: %d", DefaultCWEPort); !strings.Contains(runner.applied[len(runner.applied)-1], want) {
		t.Errorf("the published Service does not carry %q:\n%s", want, runner.applied[len(runner.applied)-1])
	}
}

func TestDeploy_ConfiguresCWEPort(t *testing.T) {
	runner := &fakeRunner{}
	cfg := testConfig(t)
	cfg.CWEPort = 30181
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	cwe := helmCalls(runner)[4]
	if !strings.Contains(cwe, "service.port=30181") {
		t.Errorf("controller helm call = %q, want configured service port", cwe)
	}
	manifest := runner.applied[len(runner.applied)-1]
	for _, want := range []string{"port: 30181", "nodePort: 30181"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the published Service does not carry %q:\n%s", want, manifest)
		}
	}
}

func TestDeploy_EnablesPostInitCNI(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	cwe := helmCalls(runner)[4]
	if !strings.Contains(cwe, "cni.installAfterInit=true") {
		t.Errorf("controller helm call = %q, want post-init CNI enabled", cwe)
	}
}

func TestDeploy_ControllerValuesCarryOIDCProviders(t *testing.T) {
	embedded, err := fs.ReadFile(manifestsFS, cweValuesFile)
	if err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if got := runner.valuesFiles[cweReleaseName]; got != string(embedded) {
		t.Errorf("controller --values file differs from the embedded %s", cweValuesFile)
	}
	if strings.Count(helmCalls(runner)[4], "--values") != 1 {
		t.Errorf("controller helm call = %q, want one --values when no providers are set", helmCalls(runner)[4])
	}

	cfg := testConfig(t)
	cfg.OIDCProviders = []sitespec.OIDCProvider{{
		Name:           "corp",
		IssuerURL:      "https://login.example.com",
		Audiences:      []string{"nvke"},
		UsernameClaim:  "email",
		UsernamePrefix: "corp:",
		CABundle:       "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
	}}
	runner = &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if got := runner.valuesFiles[cweReleaseName]; got != string(embedded) {
		t.Errorf("embedded controller values changed when providers were set")
	}
	extra := runner.extraValuesFiles[cweReleaseName]
	if len(extra) != 1 {
		t.Fatalf("oidc values files = %d, want 1", len(extra))
	}
	for _, want := range []string{"name: corp", "issuerURL: https://login.example.com", "providers:"} {
		if !strings.Contains(extra[0], want) {
			t.Errorf("oidc values missing %q:\n%s", want, extra[0])
		}
	}
	if strings.Contains(extra[0], "fullnameOverride") {
		t.Errorf("oidc values include the embedded controller values:\n%s", extra[0])
	}
	cwe := helmCalls(runner)[4]
	if strings.Count(cwe, "--values") != 2 {
		t.Errorf("controller helm call has %d --values, want 2: %s", strings.Count(cwe, "--values"), cwe)
	}
}

// TestDeploy_InstallsPostgresFromTheEmbeddedChart: Postgres comes from the
// binary, like the controller chart. Nothing is pulled, so the call carries no
// chart version. Its one values file renames the controller's database; the
// chart's own values hold the Temporal databases, the Service name both
// consumers dial, and the pinned image.
func TestDeploy_InstallsPostgresFromTheEmbeddedChart(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	postgres := helmCalls(runner)[0]

	chartArg := chartArgOf(t, postgres, postgresReleaseName)
	if !filepath.IsAbs(chartArg) {
		t.Errorf("postgres chart = %q, want the path of an extracted chart", chartArg)
	}
	if !runner.chartFiles[postgresReleaseName]["templates/postgresql.yaml"] {
		t.Errorf("the chart installed for postgres is not the embedded one: %v", runner.chartFiles[postgresReleaseName])
	}
	if strings.Contains(postgres, "--version ") {
		t.Errorf("postgres call passes --version, but nothing is pulled: %q", postgres)
	}
	if !strings.Contains(postgres, "--values ") {
		t.Errorf("postgres call = %q, want the values file that names the controller's database", postgres)
	}
	want, err := fs.ReadFile(manifestsFS, postgresValuesFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.valuesFiles[postgresReleaseName]; got != string(want) {
		t.Errorf("postgres --values file = %q, want the embedded %s", got, postgresValuesFile)
	}
	if !strings.Contains(postgres, "auth.password=test-password") {
		t.Errorf("postgres call = %q, want the password supplied per run", postgres)
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
		t.Fatalf("expected 5 helm calls (postgres, temporal, SPIRE CRDs, SPIRE, controller), got %d: %+v",
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

	if !strings.Contains(postgres, "upgrade --install postgres "+chartArgOf(t, postgres, postgresReleaseName)) {
		t.Errorf("postgres call = %q, want the postgres release/chart", postgres)
	}
	if !strings.Contains(postgres, "auth.password=test-password") {
		t.Errorf("postgres call = %q, want the postgres password set", postgres)
	}

	if !strings.Contains(temporal, "upgrade --install temporal temporal --repo https://go.temporal.io/helm-charts") ||
		!hasArgs(temporal, "--version", "0.74.0") {
		t.Errorf("temporal call = %q, want the temporal release/chart/repo/version", temporal)
	}
	if !strings.Contains(temporal, "server.config.persistence.default.sql.password=test-password") {
		t.Errorf("temporal call = %q, want the shared postgres password set on both persistence stores", temporal)
	}

	if !strings.Contains(spireCRDs, "upgrade --install spire-crds spire-crds") ||
		!strings.Contains(spireCRDs, "--repo https://spiffe.github.io/helm-charts-hardened/") ||
		!hasArgs(spireCRDs, "--version", "0.6.1") {
		t.Errorf("SPIRE CRDs call = %q, want the pinned CRD chart", spireCRDs)
	}
	if !strings.Contains(spire, "upgrade --install spire spire") ||
		!strings.Contains(spire, "global.spire.clusterName=test-kind") ||
		!strings.Contains(spire, "global.spire.trustDomain=test-kind.nke.local") ||
		!strings.Contains(spire, "spire-server.adminIDs[0]=spiffe://test-kind.nke.local/nke-service/nke-controller") {
		t.Errorf("SPIRE call = %q, want the pinned chart and controller identity", spire)
	}

	if !strings.Contains(cwe, "upgrade --install "+cweReleaseName+" ") {
		t.Errorf("controller call = %q, want the controller release and its chart directory", cwe)
	}
	if !strings.Contains(cwe, "--set-string supportedInfraProviders=infra") {
		t.Errorf("controller call = %q, want infra provider support enabled", cwe)
	}
	if !strings.Contains(cwe, "image.repository=nvcr.io/org/controller") || !strings.Contains(cwe, "image.tag=edge") {
		t.Errorf("controller call = %q, want the image split into repository/tag", cwe)
	}
	// profilesDir only. nvkectl deploys no profiles content, so the chart's
	// profiles.image stays disabled and nothing should turn it on.
	if !strings.Contains(cwe, "profiles.path="+profilesPath) {
		t.Errorf("controller call = %q, want profilesDir pointed at %s", cwe, profilesPath)
	}
	if strings.Contains(cwe, "profiles.image") {
		t.Errorf("controller call = %q, want no profiles.image setting", cwe)
	}
	if !strings.Contains(cwe, "spiffe.enabled=true") ||
		!strings.Contains(cwe, "spiffe.trustDomain=test-kind.nke.local") ||
		!strings.Contains(cwe, "spiffe.spire.serverAddress="+spireServiceHost()) ||
		!strings.Contains(cwe, "spiffe.spire.serverPort="+spireServerPort) {
		t.Errorf("controller call = %q, want SPIFFE enabled with the SPIRE trust domain", cwe)
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

// TestDeploy_UsesTheEmbeddedChartOutsideARepo is the reason the chart is
// embedded: a binary run from anywhere installs the chart it was built with,
// with no checkout above the working directory, no environment variable and
// no way to point it somewhere else.
func TestDeploy_UsesTheEmbeddedChartOutsideARepo(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chdir(outside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := testConfig(t)
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	releases := helmCalls(runner)
	// The directories are removed when Deploy returns, so what they held is
	// recorded as each call is made.
	for release, want := range map[string][]string{
		cweReleaseName:      {"Chart.yaml", "templates/_helpers.tpl"},
		postgresReleaseName: {"Chart.yaml", "templates/_helpers.tpl", "templates/postgresql.yaml"},
	} {
		files, ok := runner.chartFiles[release]
		if !ok {
			t.Fatalf("the chart installed for %s was never inspected", release)
		}
		for _, f := range want {
			if !files[f] {
				t.Errorf("the chart extracted for %s is missing %s: %v", release, f, files)
			}
		}
	}
	for _, release := range []string{cweReleaseName, postgresReleaseName} {
		chartArg := chartArgOf(t, callFor(t, releases, release), release)
		if _, err := os.Stat(chartArg); !os.IsNotExist(err) {
			t.Errorf("the chart extracted for %s outlived Deploy at %s", release, chartArg)
		}
	}
}

// callFor returns the `helm upgrade --install <release>` call from calls.
func callFor(t *testing.T, calls []string, release string) string {
	t.Helper()
	for _, call := range calls {
		if strings.Contains(call, "upgrade --install "+release+" ") {
			return call
		}
	}
	t.Fatalf("no helm install for %s in %v", release, calls)
	return ""
}

// chartArgOf returns the chart argument of a `helm upgrade --install <release>
// <chart>` call.
func chartArgOf(t *testing.T, call, release string) string {
	t.Helper()
	fields := strings.Fields(call)
	for i, f := range fields {
		if f == release && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Fatalf("no chart argument in %q", call)
	return ""
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
		t.Errorf("controller call = %q, want the digest split into repository/digest", cwe)
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

// TestSpireInstallArgs pins what nvkectl computes rather than what the values
// file carries: the chart it resolves, and the depth the computed values are
// set at. A value set at the wrong depth is silently ignored by helm rather
// than rejected, so nothing else would catch it.
func TestSpireInstallArgs(t *testing.T) {
	got := joinArgs(spireInstallArgs(Config{}, testSpireChart(t), "/tmp/values.yaml", "kind", "kind.nke.local", time.Minute))
	for _, want := range []string{
		"upgrade --install spire spire",
		"--repo https://spiffe.github.io/helm-charts-hardened/",
		"--version 0.30.1",
		"--values /tmp/values.yaml",
		"global.spire.clusterName=kind",
		"global.spire.trustDomain=kind.nke.local",
		spireServerValuesPrefix + ".adminIDs[0]=spiffe://kind.nke.local/nke-service/nke-controller",
	} {
		if !hasArgs(got, strings.Fields(want)...) {
			t.Errorf("install = %q, want it to contain %q", got, want)
		}
	}
}

// TestUpstreamCAArgs covers both states: a CA from the site spec is chained to,
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
		Config{TPMEKFingerprints: []string{"aaa"}}, "spire-server",
	)), "service.type") {
		t.Error("tpmAttestorArgs should not change the chart's Service")
	}

	none := tpmAttestorArgs(Config{}, "spire-server")
	if len(none) != 0 {
		t.Errorf("tpmAttestorArgs with no fingerprints = %v, want none: a bootstrap that "+
			"has not been told which hardware to admit must admit none", none)
	}

	got := joinArgs(tpmAttestorArgs(
		Config{TPMEKFingerprints: []string{"aaa", "bbb"}}, "spire.spire-server",
	))
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

// TestSpireAndCWEAgreeOnTheClassName is the guard for a mismatch neither side
// reports. The NVKE controller stamps its ClusterStaticEntry objects with its configured class
// and the SPIRE controller manager only acts on objects carrying its own, so
// two different strings mean the NVKE controller writes entries nobody reads and a node never
// gets an identity -- with both components healthy and nothing logged.
//
// Neither default reaches the same string on its own: the chart derives
// <namespace>-<release>, the NVKE controller defaults to spire-mgmt-spire.
//
// The expected class is spelled out rather than taken from the constant, for
// the same reason the port test spells out its address: asserting the constant
// against itself would survive any drift. This pins our value to the literal
// the NVKE controller's spire.className defaults to. It cannot catch the NVKE controller changing that default
// -- that lives in another module -- but it does catch this side moving.
func TestSpireAndCWEAgreeOnTheClassName(t *testing.T) {
	const cweClassNameDefault = "spire-mgmt-spire"

	if spireControllerManagerClass != cweClassNameDefault {
		t.Errorf("the class is %q, but the NVKE controller's spire.className defaults to %q; "+
			"the NVKE controller would write ClusterStaticEntry objects the controller manager ignores",
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
		case strings.Contains(args, "upgrade --install "+spireReleaseName+" spire"):
			spire = args
		case strings.Contains(args, "upgrade --install "+cweReleaseName):
			cwe = args
		}
	}
	if spire == "" || cwe == "" {
		t.Fatalf("expected both installs; spire=%q controller=%q", spire, cwe)
	}

	if !strings.Contains(spire,
		spireServerValuesPrefix+".controllerManager.className="+spireControllerManagerClass) {
		t.Errorf("SPIRE install does not pin the controller-manager class: %s", spire)
	}
	if !strings.Contains(cwe, "spiffe.spire.className="+spireControllerManagerClass) {
		t.Errorf("controller install does not pin the same class: %s", cwe)
	}
}

// TestSpireServesThePortCWEExpects covers the other silent mismatch: the controller reads
// SPIRE_SERVER_API_ADDRESS for join-token calls and nvkectl never sets it, so
// the Service has to listen where that default already points. The literal is
// spelled out rather than built from spireServerPort, because the thing under
// test is that our constant still matches the controller's default -- writing both sides
// from the same constant would assert nothing.
func TestSpireServesThePortCWEExpects(t *testing.T) {
	const cweAPIAddressDefault = "spire-server.spire-server.svc.cluster.local:8081"

	if got := spireServiceHost() + ":" + spireServerPort; got != cweAPIAddressDefault {
		t.Errorf("the SPIRE Service is at %q, but the controller's SPIRE_SERVER_API_ADDRESS defaults to %q; "+
			"join-token operations would dial a port nothing serves", got, cweAPIAddressDefault)
	}

	args := joinArgs(spireInstallArgs(Config{}, testSpireChart(t), "/tmp/values.yaml", "kind", "kind.nke.local", time.Minute))
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

// DisableMTLS stands the site up without SPIFFE: no SPIRE release at all, and
// the controller installed with spiffe.enabled=false, which is what makes it dial each
// node agent in plaintext.
func TestDeploy_DisableMTLSSkipsSpire(t *testing.T) {
	runner := &fakeRunner{}
	cfg := testConfig(t)
	cfg.DisableMTLS = true
	cfg.TPMEKFingerprints = []string{"aaa"}

	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	all := strings.Join(helmCalls(runner), "\n")
	if strings.Contains(all, "spire-crds") || strings.Contains(all, "upgrade --install spire ") {
		t.Errorf("calls = %q, want no SPIRE release when mTLS is disabled", all)
	}
	if strings.Contains(all, "nodeAttestor.tpmDirect.enabled=true") {
		t.Errorf("calls = %q, want no TPM attestor when mTLS is disabled", all)
	}
	if !strings.Contains(all, "spiffe.enabled=false") {
		t.Errorf("calls = %q, want the controller installed with spiffe.enabled=false", all)
	}
}

// An override replaces the chart half of the install outright: the chart
// argument is used verbatim and --repo goes with it, since helm resolves a
// name through --repo and would send an OCI reference to the repo's index
// instead of pulling it. The version pin stays either way.
func TestSpireInstallArgs_PullsTheChartFromTheConfiguredRef(t *testing.T) {
	for _, tt := range []struct {
		name      string
		ref       string
		wantChart string
		wantRepo  bool
	}{
		{name: "default is the hardened chart repo", ref: "", wantChart: "spire", wantRepo: true},
		{name: "OCI reference is used verbatim without --repo", ref: "oci://mirror.example.com/spiffe/spire", wantChart: "oci://mirror.example.com/spiffe/spire"},
		{name: "local tarball is used verbatim without --repo", ref: "/srv/charts/spire-0.30.1.tgz", wantChart: "/srv/charts/spire-0.30.1.tgz"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := joinArgs(spireInstallArgs(Config{SPIREChartRef: tt.ref}, testSpireChart(t), "/tmp/values.yaml", "kind", "kind.nke.local", time.Minute))
			if !strings.HasPrefix(got, "upgrade --install spire "+tt.wantChart+" ") {
				t.Errorf("install = %q, want chart %q", got, tt.wantChart)
			}
			if gotRepo := strings.Contains(got, "--repo "); gotRepo != tt.wantRepo {
				t.Errorf("install = %q, has --repo = %v, want %v", got, gotRepo, tt.wantRepo)
			}
			if !hasArgs(got, "--version", "0.30.1") {
				t.Errorf("install = %q, want the version pin kept", got)
			}
		})
	}
}

// Deploy builds the Temporal and SPIRE CRDs installs inline rather than
// through a helper, so those two are asserted on the recorded helm calls.
// SPIRE is included so one table pins all three chart overrides side by side.
func TestDeploy_PullsTemporalAndSPIREChartsFromTheConfiguredRef(t *testing.T) {
	const mirror = "oci://mirror.example.com/charts/"
	for _, tt := range []struct {
		name  string
		set   func(*Config, string)
		call  int // index into helmCalls: postgres, temporal, SPIRE CRDs, SPIRE, controller
		chart string
		repo  string
	}{
		{name: temporalReleaseName, set: func(c *Config, ref string) { c.TemporalChartRef = ref }, call: 1, chart: "temporal", repo: "https://go.temporal.io/helm-charts"},
		{name: spireCRDsReleaseName, set: func(c *Config, ref string) { c.SPIRECRDsChartRef = ref }, call: 2, chart: "spire-crds", repo: "https://spiffe.github.io/helm-charts-hardened/"},
		{name: spireReleaseName, set: func(c *Config, ref string) { c.SPIREChartRef = ref }, call: 3, chart: "spire", repo: "https://spiffe.github.io/helm-charts-hardened/"},
	} {
		t.Run(tt.name+" defaults to the upstream repo", func(t *testing.T) {
			runner := &fakeRunner{}
			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
				t.Fatalf("Deploy returned error: %v", err)
			}
			got := helmCalls(runner)[tt.call]
			want := "upgrade --install " + tt.name + " " + tt.chart + " --repo " + tt.repo + " "
			if !strings.Contains(got, want) {
				t.Errorf("%s helm call = %q, want it to contain %q", tt.name, got, want)
			}
		})
		t.Run(tt.name+" override is used verbatim without --repo", func(t *testing.T) {
			runner := &fakeRunner{}
			cfg := testConfig(t)
			ref := mirror + tt.chart
			tt.set(&cfg, ref)
			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
				t.Fatalf("Deploy returned error: %v", err)
			}
			calls := helmCalls(runner)
			got := calls[tt.call]
			if !strings.Contains(got, "upgrade --install "+tt.name+" "+ref+" ") {
				t.Errorf("%s helm call = %q, want chart %q", tt.name, got, ref)
			}
			if strings.Contains(got, "--repo") {
				t.Errorf("%s helm call = %q, want no --repo alongside an override", tt.name, got)
			}
			// One override moves one chart; the other releases keep their source.
			for i, other := range calls {
				if i != tt.call && strings.Contains(other, ref) {
					t.Errorf("helm call[%d] = %q, carries the %s override", i, other, tt.name)
				}
			}
		})
	}
}

// TestDefaultConfig_ImagesDefaultToTheManifest: with neither variable set the
// two built images are the manifest's refs; a set variable wins.
func TestDefaultConfig_ImagesDefaultToTheManifest(t *testing.T) {
	t.Setenv("NVKECTL_CONTROLLER_IMAGE", "")
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "")
	cfg := DefaultConfig()
	m := manifest.Load()
	if want := m.Components[manifest.ComponentController].Images[manifest.ImageController].Ref; cfg.CWEImage != want || want == "" {
		t.Errorf("CWEImage = %q, want the manifest's %q", cfg.CWEImage, want)
	}
	if want := m.Components[manifest.ComponentProvisioned].Images[manifest.ImageManager].Ref; cfg.ProvisionedProviderImage != want || want == "" {
		t.Errorf("ProvisionedProviderImage = %q, want the manifest's %q", cfg.ProvisionedProviderImage, want)
	}

	t.Setenv("NVKECTL_CONTROLLER_IMAGE", "nvcr.io/org/nvke-controller:edge")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "nvcr.io/org/provisioned:edge")
	cfg = DefaultConfig()
	if cfg.CWEImage != "nvcr.io/org/nvke-controller:edge" || cfg.ProvisionedProviderImage != "nvcr.io/org/provisioned:edge" {
		t.Errorf("environment did not win: %+v", cfg)
	}
}

func TestDefaultConfig_ChartRefs(t *testing.T) {
	t.Setenv("NVKECTL_CONTROLLER_TEMPORAL_CHART", "/srv/charts/temporal-0.74.0.tgz")
	t.Setenv("NVKECTL_CONTROLLER_SPIRE_CHART", "oci://mirror.example.com/spiffe/spire")
	t.Setenv("NVKECTL_CONTROLLER_SPIRE_CRDS_CHART", "oci://mirror.example.com/spiffe/spire-crds")

	cfg := DefaultConfig()
	for _, tt := range []struct{ field, got, want string }{
		{"TemporalChartRef", cfg.TemporalChartRef, "/srv/charts/temporal-0.74.0.tgz"},
		{"SPIREChartRef", cfg.SPIREChartRef, "oci://mirror.example.com/spiffe/spire"},
		{"SPIRECRDsChartRef", cfg.SPIRECRDsChartRef, "oci://mirror.example.com/spiffe/spire-crds"},
	} {
		if tt.got != tt.want {
			t.Errorf("DefaultConfig %s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}
}

// renamedSettingCases is every setting this package reads, with its previous
// NKECTL_* name, two values it accepts and the Config field that carries it.
var renamedSettingCases = []struct {
	name, old  string
	oldV, newV string
	got        func(Config) string
}{
	{"NVKECTL_CONTROLLER_IMAGE", "NKECTL_CONTROLLER_IMAGE", "nvcr.io/org/old:1", "nvcr.io/org/new:1", func(c Config) string { return c.CWEImage }},
	{"NVKECTL_CONTROLLER_PORT", "NKECTL_CONTROLLER_PORT", "30181", "30182", func(c Config) string { return strconv.Itoa(int(c.CWEPort)) }},
	{"NVKECTL_CONTROLLER_HELM_TIMEOUT", "NKECTL_CONTROLLER_HELM_TIMEOUT", "1m0s", "2m0s", func(c Config) string { return c.HelmTimeout.String() }},
	{"NVKECTL_CONTROLLER_READY_TIMEOUT", "NKECTL_CONTROLLER_READY_TIMEOUT", "3m0s", "4m0s", func(c Config) string { return c.ReadyTimeout.String() }},
	{"NVKECTL_CONTROLLER_TEMPORAL_CHART", "NKECTL_CONTROLLER_TEMPORAL_CHART", "/srv/old/temporal.tgz", "/srv/new/temporal.tgz", func(c Config) string { return c.TemporalChartRef }},
	{"NVKECTL_CONTROLLER_SPIRE_CHART", "NKECTL_CONTROLLER_SPIRE_CHART", "oci://old.example.com/spire", "oci://new.example.com/spire", func(c Config) string { return c.SPIREChartRef }},
	{"NVKECTL_CONTROLLER_SPIRE_CRDS_CHART", "NKECTL_CONTROLLER_SPIRE_CRDS_CHART", "oci://old.example.com/spire-crds", "oci://new.example.com/spire-crds", func(c Config) string { return c.SPIRECRDsChartRef }},
	{"NVKECTL_CAPI_PROVISIONED_IMAGE", "NKECTL_CAPI_PROVISIONED_IMAGE", "nvcr.io/org/old-provisioned:1", "nvcr.io/org/new-provisioned:1", func(c Config) string { return c.ProvisionedProviderImage }},
	{"NVKECTL_SPIFFE_TRUST_DOMAIN", "NKECTL_SPIFFE_TRUST_DOMAIN", "old.example", "new.example", func(c Config) string { return c.SPIFFETrustDomain }},
}

// TestDefaultConfig_RenamedSettings: each NVKECTL_CONTROLLER_* setting is
// read; its NKECTL_* name is read when the new one is unset and is
// then reported as deprecated; with both set the new one wins and nothing is
// reported.
func TestDefaultConfig_RenamedSettings(t *testing.T) {
	if len(renamedSettingCases) != len(Settings) {
		t.Fatalf("%d cases for %d settings", len(renamedSettingCases), len(Settings))
	}
	for _, tc := range renamedSettingCases {
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
				for _, c := range renamedSettingCases {
					t.Setenv(c.name, "")
					t.Setenv(c.old, "")
				}
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

// TestDeprecatedSettings_SortedByOldName: several old names at once come back
// in a stable order, so the warnings print the same way every run.
func TestDeprecatedSettings_SortedByOldName(t *testing.T) {
	for _, c := range renamedSettingCases {
		t.Setenv(c.name, "")
		t.Setenv(c.old, "")
	}
	t.Setenv("NKECTL_CONTROLLER_SPIRE_CHART", "oci://old.example.com/spire")
	t.Setenv("NKECTL_CONTROLLER_IMAGE", "nvcr.io/org/old:1")
	t.Setenv("NKECTL_CONTROLLER_PORT", "30181")

	want := []envalias.Renamed{
		{Old: "NKECTL_CONTROLLER_IMAGE", New: "NVKECTL_CONTROLLER_IMAGE"},
		{Old: "NKECTL_CONTROLLER_PORT", New: "NVKECTL_CONTROLLER_PORT"},
		{Old: "NKECTL_CONTROLLER_SPIRE_CHART", New: "NVKECTL_CONTROLLER_SPIRE_CHART"},
	}
	if got := DeprecatedSettings(); !slices.Equal(got, want) {
		t.Errorf("DeprecatedSettings = %v, want %v", got, want)
	}
}

// removedSettingNames is every controller setting name nkectl cli/v0.1.x read.
var removedSettingNames = []string{
	"NKECTL_CWE_HELM_TIMEOUT",
	"NKECTL_CWE_IMAGE",
	"NKECTL_CWE_PORT",
	"NKECTL_CWE_READY_TIMEOUT",
	"NKECTL_CWE_SPIRE_CHART",
	"NKECTL_CWE_SPIRE_CRDS_CHART",
	"NKECTL_CWE_TEMPORAL_CHART",
}

// TestRemovedSettings: every cli/v0.1.x name that is set comes back, sorted,
// with the setting that replaces it; one that is unset does not, and setting
// its replacement does not hide it.
func TestRemovedSettings(t *testing.T) {
	for _, old := range removedSettingNames {
		t.Setenv(old, "")
	}
	if got := RemovedSettings(); len(got) != 0 {
		t.Fatalf("RemovedSettings with nothing set = %v", got)
	}

	t.Setenv("NKECTL_CWE_SPIRE_CHART", "oci://old.example.com/spire")
	t.Setenv("NKECTL_CWE_IMAGE", "nvcr.io/org/old:1")
	t.Setenv("NVKECTL_CONTROLLER_IMAGE", "nvcr.io/org/new:1")
	want := []envalias.Renamed{
		{Old: "NKECTL_CWE_IMAGE", New: "NVKECTL_CONTROLLER_IMAGE"},
		{Old: "NKECTL_CWE_SPIRE_CHART", New: "NVKECTL_CONTROLLER_SPIRE_CHART"},
	}
	if got := RemovedSettings(); !slices.Equal(got, want) {
		t.Errorf("RemovedSettings = %v, want %v", got, want)
	}

	for _, old := range removedSettingNames {
		t.Setenv(old, "x")
	}
	var olds []string
	for _, r := range RemovedSettings() {
		if !slices.Contains(Settings, r.New) {
			t.Errorf("%s maps to %s, which DefaultConfig does not read", r.Old, r.New)
		}
		olds = append(olds, r.Old)
	}
	if !slices.Equal(olds, removedSettingNames) {
		t.Errorf("RemovedSettings covers %v, want %v", olds, removedSettingNames)
	}
}

// TestDeploy_ChartsMatchTheBootstrapManifest: every chart Deploy pulls is one
// the embedded bootstrap manifest pins, and every chart the manifest pins is
// pulled. Identities live in the manifest alone; this test fails when either
// side gains or loses a chart.
func TestDeploy_ChartsMatchTheBootstrapManifest(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	pinned := map[string]manifest.Chart{}
	for name, comp := range manifest.Load().Components {
		if comp.Chart != nil {
			pinned[name] = *comp.Chart
		}
	}

	pulled := 0
	for _, call := range helmCalls(runner) {
		if !strings.Contains(call, " --repo ") {
			continue // embedded charts install from a path and pull nothing
		}
		pulled++
		matched := false
		for name, chart := range pinned {
			if hasArgs(call, chart.Name, "--repo", chart.Repo) && hasArgs(call, "--version", chart.Version) {
				delete(pinned, name)
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("Deploy pulls a chart the bootstrap manifest does not pin: %q", call)
		}
	}
	for name, chart := range pinned {
		t.Errorf("the bootstrap manifest pins %s (%s %s from %s) but Deploy never pulls it", name, chart.Name, chart.Version, chart.Repo)
	}
	if pulled == 0 {
		t.Fatal("no pulled charts found; the assertion matched nothing")
	}
}

// testSpireChart is the SPIRE chart as the embedded bootstrap manifest pins it.
func testSpireChart(t *testing.T) manifest.Chart {
	t.Helper()
	comp, ok := manifest.Load().Components[manifest.ComponentSPIRE]
	if !ok || comp.Chart == nil {
		t.Fatal("the embedded bootstrap manifest pins no spire chart")
	}
	return *comp.Chart
}

// hasArgs reports whether call holds args as adjacent whole tokens.
func hasArgs(call string, args ...string) bool {
	fields := strings.Fields(call)
	for i := 0; i+len(args) <= len(fields); i++ {
		if slices.Equal(fields[i:i+len(args)], args) {
			return true
		}
	}
	return false
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
