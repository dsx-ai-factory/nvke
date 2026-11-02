// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
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

// secretValue returns the value at path in the --values files release was
// installed with, or "" when none sets it.
func (f *fakeRunner) secretValue(t *testing.T, release string, path ...string) string {
	t.Helper()
	for _, body := range append([]string{f.valuesFiles[release]}, f.extraValuesFiles[release]...) {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("a %s values file is not YAML: %v", release, err)
		}
		var cur any = doc
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			cur = m[key]
		}
		if v, ok := cur.(string); ok {
			return v
		}
	}
	return ""
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

// TestDeploy_ControllerValuesCarryOIDCProviders verifies providers reach Helm without changing embedded values.
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
	// The embedded values, then the file holding the generated password.
	if strings.Count(helmCalls(runner)[4], "--values") != 2 {
		t.Errorf("controller helm call = %q, want two --values when no providers are set", helmCalls(runner)[4])
	}

	cfg := testConfig(t)
	usernamePrefix := "corp:"
	cfg.OIDCProviders = []sitespec.OIDCProvider{{
		Name:           "corp",
		IssuerURL:      "https://login.example.com",
		Audiences:      []string{"nvke"},
		UsernameClaim:  "email",
		UsernamePrefix: &usernamePrefix,
		CABundle:       "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		DiscoveryURL:   "https://identity.example.com/.well-known/openid-configuration",
	}}
	runner = &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if got := runner.valuesFiles[cweReleaseName]; got != string(embedded) {
		t.Errorf("embedded controller values changed when providers were set")
	}
	// After the embedded values come the generated password's file, then the
	// providers' last.
	extra := runner.extraValuesFiles[cweReleaseName]
	if len(extra) != 2 {
		t.Fatalf("extra controller values files = %d, want the password's and the oidc one", len(extra))
	}
	oidc := extra[1]
	var rendered struct {
		OIDC struct {
			Providers []struct {
				IssuerURL    string `yaml:"issuerURL"`
				DiscoveryURL string `yaml:"discoveryURL"`
			} `yaml:"providers"`
		} `yaml:"oidc"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(oidc), &rendered))
	require.Len(t, rendered.OIDC.Providers, 1)
	assert.Equal(t, cfg.OIDCProviders[0].IssuerURL, rendered.OIDC.Providers[0].IssuerURL)
	assert.Equal(t, cfg.OIDCProviders[0].DiscoveryURL, rendered.OIDC.Providers[0].DiscoveryURL)
	for _, want := range []string{"name: corp", "issuerURL: https://login.example.com", "providers:"} {
		if !strings.Contains(oidc, want) {
			t.Errorf("oidc values missing %q:\n%s", want, oidc)
		}
	}
	if strings.Contains(oidc, "fullnameOverride") {
		t.Errorf("oidc values include the embedded controller values:\n%s", oidc)
	}
	if strings.Contains(oidc, "postgresPassword") {
		t.Errorf("oidc values include the generated password:\n%s", oidc)
	}
	cwe := helmCalls(runner)[4]
	if strings.Count(cwe, "--values") != 3 {
		t.Errorf("controller helm call has %d --values, want 3: %s", strings.Count(cwe, "--values"), cwe)
	}
}

// TestDeploy_OmitsUnsetOIDCDiscoveryURL verifies an unset override preserves default discovery behavior.
func TestDeploy_OmitsUnsetOIDCDiscoveryURL(t *testing.T) {
	cfg := testConfig(t)
	cfg.OIDCProviders = []sitespec.OIDCProvider{{Name: "corp", IssuerURL: "https://login.example.com"}}
	runner := &fakeRunner{}
	require.NoError(t, New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg))
	extra := runner.extraValuesFiles[cweReleaseName]
	require.Len(t, extra, 2)
	assert.NotContains(t, extra[1], "discoveryURL:")
}

// TestDeploy_EmptyOIDCUsernamePrefix verifies Helm receives an explicit empty username prefix.
func TestDeploy_EmptyOIDCUsernamePrefix(t *testing.T) {
	cfg := testConfig(t)
	usernamePrefix := ""
	cfg.OIDCProviders = []sitespec.OIDCProvider{{Name: "corp", IssuerURL: "https://login.example.com", UsernamePrefix: &usernamePrefix}}
	runner := &fakeRunner{}
	require.NoError(t, New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg))
	extra := runner.extraValuesFiles[cweReleaseName]
	require.Len(t, extra, 2)
	var rendered struct {
		OIDC struct {
			Providers []struct {
				UsernamePrefix *string `yaml:"usernamePrefix"`
			} `yaml:"providers"`
		} `yaml:"oidc"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(extra[1]), &rendered))
	require.Len(t, rendered.OIDC.Providers, 1)
	require.NotNil(t, rendered.OIDC.Providers[0].UsernamePrefix)
	assert.Empty(t, *rendered.OIDC.Providers[0].UsernamePrefix)
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
	if got := runner.secretValue(t, postgresReleaseName, "auth", "password"); got != "test-password" {
		t.Errorf("postgres auth.password in its values files = %q, want the password supplied per run", got)
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
	if got := runner.secretValue(t, postgresReleaseName, "auth", "password"); got != "test-password" {
		t.Errorf("postgres auth.password in its values files = %q, want the postgres password set", got)
	}

	if !strings.Contains(temporal, "upgrade --install temporal temporal --repo https://go.temporal.io/helm-charts") ||
		!hasArgs(temporal, "--version", "0.74.0") {
		t.Errorf("temporal call = %q, want the temporal release/chart/repo/version", temporal)
	}
	for _, store := range []string{"default", "visibility"} {
		if got := runner.secretValue(t, temporalReleaseName, "server", "config", "persistence", store, "sql", "password"); got != "test-password" {
			t.Errorf("temporal %s store password in its values files = %q, want the shared postgres password", store, got)
		}
	}

	if !strings.Contains(spireCRDs, "upgrade --install spire-crds spire-crds") ||
		!strings.Contains(spireCRDs, "--repo https://spiffe.github.io/helm-charts-hardened/") ||
		!hasArgs(spireCRDs, "--version", "0.6.1") {
		t.Errorf("SPIRE CRDs call = %q, want the pinned CRD chart", spireCRDs)
	}
	if !strings.Contains(spire, "upgrade --install spire spire") ||
		!strings.Contains(spire, "global.spire.clusterName=test-kind") ||
		!strings.Contains(spire, "global.spire.trustDomain=test-kind.nke.local") ||
		!strings.Contains(spire, "spire-server.adminIDs[0]=spiffe://test-kind.nke.local/nvke-service/nvke-controller") {
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
	wantProxyPath := "/api/v1/namespaces/nvke-controller/services/nvke-controller:http/proxy/readyz"
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
		spireServerValuesPrefix + ".adminIDs[0]=spiffe://kind.nke.local/nvke-service/nvke-controller",
	} {
		if !hasArgs(got, strings.Fields(want)...) {
			t.Errorf("install = %q, want it to contain %q", got, want)
		}
	}
}

// TestUpstreamCAArgs covers both states: a CA from the environment is chained to,
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

func TestDefaultConfig_ChartRefs(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART", "/srv/charts/temporal-0.74.0.tgz")
	t.Setenv("NVKECTL_NVKE_CONTROLLER_SPIRE_CHART", "oci://mirror.example.com/spiffe/spire")
	t.Setenv("NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART", "oci://mirror.example.com/spiffe/spire-crds")

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
	{"NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART", "", "/srv/old/temporal.tgz", "/srv/new/temporal.tgz", func(c Config) string { return c.TemporalChartRef }},
	{"NVKECTL_NVKE_CONTROLLER_SPIRE_CHART", "", "oci://old.example.com/spire", "oci://new.example.com/spire", func(c Config) string { return c.SPIREChartRef }},
	{"NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART", "", "oci://old.example.com/spire-crds", "oci://new.example.com/spire-crds", func(c Config) string { return c.SPIRECRDsChartRef }},
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

// TestDeploy_PasswordsAreNotArguments checks where the generated password goes:
// into a values file for each of the three releases that receive it, and into no
// command-line argument, where it would show in `ps` and in the command an
// error prints.
func TestDeploy_PasswordsAreNotArguments(t *testing.T) {
	cfg := testConfig(t)
	cfg.PostgresPassword = fakeValue
	runner := &fakeRunner{}

	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	for _, call := range runner.calls {
		for _, arg := range call.args {
			if strings.Contains(arg, fakeValue) {
				t.Errorf("%s argument %q holds the password", call.name, arg)
			}
		}
	}
	for release, path := range map[string][]string{
		postgresReleaseName: {"auth", "password"},
		cweReleaseName:      {"appDb", "postgresPassword"},
	} {
		if got := runner.secretValue(t, release, path...); got != fakeValue {
			t.Errorf("%s %v in its values files = %q, want the generated password", release, path, got)
		}
	}
	for _, store := range []string{"default", "visibility"} {
		if got := runner.secretValue(t, temporalReleaseName, "server", "config", "persistence", store, "sql", "password"); got != fakeValue {
			t.Errorf("temporal %s store password in its values files = %q, want the generated password", store, got)
		}
	}
}

// TestDeploy_ControllerReadsItsDatabaseURLFromASecret guards the flag that keeps
// the password out of the controller's pod spec, and so out of the diagnostics
// dump: the chart then builds the URL into a Secret and the pod refers to it.
func TestDeploy_ControllerReadsItsDatabaseURLFromASecret(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var controller string
	for _, call := range helmCalls(runner) {
		if strings.Contains(call, "upgrade --install "+cweReleaseName+" ") {
			controller = call
		}
	}
	if !hasArgs(controller, "--set", "secrets.createDbSecret=true") {
		t.Errorf("controller call = %q, want --set secrets.createDbSecret=true", controller)
	}
}

// TestDeploy_FailedInstallPrintsNoPassword fails each of the three releases that
// receive the generated password, with helm output that echoes it, and checks
// the error, which reaches the terminal, does not hold it.
func TestDeploy_FailedInstallPrintsNoPassword(t *testing.T) {
	for _, tc := range []struct {
		release string
		failAt  int // index of the helm call that fails
	}{
		{"postgres", 0},
		{"temporal", 1},
		{"controller", 4},
	} {
		t.Run(tc.release, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.PostgresPassword = fakeValue
			runner := &failingHelmRunner{fakeRunner: &fakeRunner{}, failAt: tc.failAt}

			err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg)
			if err == nil {
				t.Fatal("expected the install to fail")
			}
			if strings.Contains(err.Error(), fakeValue) {
				t.Errorf("the error holds the password:\n%s", err)
			}
			if !strings.Contains(err.Error(), "--namespace") {
				t.Errorf("the failed command should stay readable:\n%s", err)
			}
			if !strings.Contains(err.Error(), redact.Placeholder) {
				t.Errorf("helm's output echoed the password, so the error should show it masked:\n%s", err)
			}
		})
	}
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
		sub := filepath.Join(args[i+1], "nvke-controller")
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
		body, err := os.ReadFile(filepath.Join(dir, "nvke-controller", file))
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
	body, err := os.ReadFile(filepath.Join(dir, "nvke-controller", "pods.json"))
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
		"app.env":           "DATABASE_URL=postgres://u:p@h/db\n",
		"old.log":           "rejected " + fakeValue + "\n",
		"sub/notes.txt":     "postgresql://dev:hunter2@localhost/app\n",
		"nvke-controller/x": "a file in a directory the bundle also uses\n",
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
	body, err := os.ReadFile(filepath.Join(dir, "nvke-controller", "pods.json"))
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

// The password is on disk while an install runs, so it must be gone however the
// install ends, including when an install fails and Deploy returns early.
func TestDeploy_RemovesTheSecretValuesFilesWhenAnInstallFails(t *testing.T) {
	for _, tc := range []struct {
		release string
		failAt  int // index of the helm call that fails
	}{
		{"postgres", 0},
		{"temporal", 1},
		{"controller", 4},
	} {
		t.Run(tc.release, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			cfg := testConfig(t)
			cfg.PostgresPassword = fakeValue

			err := New(&failingHelmRunner{fakeRunner: &fakeRunner{}, failAt: tc.failAt}, nil).
				Deploy(context.Background(), "/tmp/kubeconfig", cfg)
			if err == nil {
				t.Fatal("expected the install to fail")
			}
			var left []string
			_ = filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					if b, _ := os.ReadFile(path); strings.Contains(string(b), fakeValue) {
						left = append(left, path)
					}
				}
				return nil
			})
			if len(left) > 0 {
				t.Errorf("files holding the password were left behind: %v", left)
			}
		})
	}
}

func TestWriteSecretValues_IsPrivateAndRemovedByItsCleanup(t *testing.T) {
	path, cleanup, err := writeSecretValues(map[string]any{"auth": map[string]any{"password": fakeValue}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600: the file holds the password", info.Mode().Perm())
	}
	cleanup()
	if _, err := os.Stat(path); err == nil {
		t.Error("the file was not removed by its cleanup")
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
			if err := os.Symlink(elsewhere, filepath.Join(dir, "nvke-controller")); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
		}},
		{"a file where a directory is wanted", func(t *testing.T, dir, _ string) {
			if err := os.WriteFile(filepath.Join(dir, "nvke-controller"), []byte("not a directory"), 0o600); err != nil {
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

	for _, name := range []string{"helm-releases.txt", filepath.Join("nvke-controller", "pods.json")} {
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

func TestProfilesImageArgs(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		want    []string // substrings the args must contain; empty means no args at all
		wantErr string
	}{
		{"unset adds nothing", "", nil, ""},
		{
			"a tagged local image",
			"controller-profiles:e2e",
			[]string{"profiles.image.enabled=true", "profiles.image.repository=controller-profiles", "profiles.image.tag=e2e", "profiles.image.digest="},
			"",
		},
		{
			"a registry image with a port and a tag",
			"localhost:5000/team/profiles:v1",
			[]string{"profiles.image.repository=localhost:5000/team/profiles", "profiles.image.tag=v1"},
			"",
		},
		{
			"a digest pin needs no tag",
			"registry.example.com/profiles@sha256:abc123",
			[]string{"profiles.image.repository=registry.example.com/profiles", "profiles.image.digest=sha256:abc123"},
			"",
		},
		{"an explicit latest tag is refused", "controller-profiles:latest", nil, "other than latest"},
		{"an untagged reference reads as latest and is refused", "controller-profiles", nil, "other than latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := profilesImageArgs(Config{ProfilesImage: tt.image})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("profilesImageArgs error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("profilesImageArgs: %v", err)
			}
			if len(tt.want) == 0 && len(args) != 0 {
				t.Fatalf("args = %v, want none", args)
			}
			joined := strings.Join(args, " ")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("args = %q, want %q", joined, want)
				}
			}
		})
	}
}

// TestDeploy_ProfilesImageReachesTheControllerInstall: the chart values are on
// the release that runs the controller, not just built.
func TestDeploy_ProfilesImageReachesTheControllerInstall(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProfilesImage = "controller-profiles:e2e"
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	install := joinArgs(runner.calls[callIndex(runner, "upgrade --install "+cweReleaseName)].args)
	for _, want := range []string{"profiles.image.enabled=true", "profiles.image.repository=controller-profiles", "profiles.image.tag=e2e"} {
		if !strings.Contains(install, want) {
			t.Errorf("controller install = %q, want %q", install, want)
		}
	}
}

func TestDeploy_RefusesAProfilesImageTheClusterWouldPull(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProfilesImage = "controller-profiles:latest"
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err == nil {
		t.Fatal("Deploy accepted a latest-tagged profiles image")
	}
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

func TestDeployControlPlaneVIPOverride(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.AllowControlPlaneWithoutVIP = allow
			runner := &fakeRunner{}
			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
				t.Fatal(err)
			}
			i := callIndex(runner, "upgrade --install "+cweReleaseName)
			if i < 0 {
				t.Fatal("no NVKE controller Helm install")
			}
			want := "--set allowControlPlaneWithoutVIP=" + strconv.FormatBool(allow)
			if !strings.Contains(joinArgs(runner.calls[i].args), want) {
				t.Fatalf("NVKE controller Helm install did not contain %q", want)
			}
		})
	}
}

// A tracing endpoint turns on the controller's own span export; without one, no
// tracing value is set. With headers too, they reach the controller as
// OTEL_EXPORTER_OTLP_HEADERS in a values file, never as an argument, alongside
// its other environment settings.
func TestDeploy_TracingValues(t *testing.T) {
	const token = "Bearer s3cr3t,token=with%odd chars"
	headers := map[string]string{"authorization": token}
	encoded := telemetry.FormatHeaders(headers)
	tests := []struct {
		name          string
		tracing       TracingConfig
		nodeAgentPort string
		want          []string
		wantEnv       map[string]string
	}{
		{
			name:    "secure endpoint",
			tracing: TracingConfig{Endpoint: "collector.example:4317"},
			want:    []string{"--set tracing.enabled=true", "--set-string tracing.endpoint=collector.example:4317", "--set tracing.insecure=false"},
		},
		{
			name:    "insecure endpoint",
			tracing: TracingConfig{Endpoint: "collector.example:4317", Insecure: true},
			want:    []string{"--set tracing.enabled=true", "--set-string tracing.endpoint=collector.example:4317", "--set tracing.insecure=true"},
		},
		{
			name:          "endpoint and headers",
			tracing:       TracingConfig{Endpoint: "collector.example:4317", Headers: headers},
			nodeAgentPort: "9444",
			want:          []string{"--set tracing.enabled=true", "--set-string tracing.endpoint=collector.example:4317", "--set tracing.insecure=false"},
			wantEnv:       map[string]string{"NODE_AGENT_PORT": "9444", "OTEL_EXPORTER_OTLP_HEADERS": encoded},
		},
		{
			name:          "insecure endpoint and headers",
			tracing:       TracingConfig{Endpoint: "collector.example:4317", Insecure: true, Headers: headers},
			nodeAgentPort: "9444",
			want:          []string{"--set tracing.enabled=true", "--set-string tracing.endpoint=collector.example:4317", "--set tracing.insecure=true"},
			wantEnv:       map[string]string{"NODE_AGENT_PORT": "9444", "OTEL_EXPORTER_OTLP_HEADERS": encoded},
		},
		{
			name:          "endpoint without headers keeps the env arguments",
			tracing:       TracingConfig{Endpoint: "collector.example:4317"},
			nodeAgentPort: "9444",
			want: []string{
				"--set tracing.enabled=true", "--set-string tracing.endpoint=collector.example:4317",
				"--set-string env[0].name=NODE_AGENT_PORT", "--set-string env[0].value=9444",
			},
		},
		{
			name:          "headers without an endpoint keep the env arguments",
			tracing:       TracingConfig{Headers: headers},
			nodeAgentPort: "9444",
			want:          []string{"--set-string env[0].name=NODE_AGENT_PORT", "--set-string env[0].value=9444"},
		},
		{name: "zero value", tracing: TracingConfig{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			cfg := testConfig(t)
			cfg.Tracing = tt.tracing
			cfg.NodeAgentPort = tt.nodeAgentPort

			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
				t.Fatalf("Deploy: %v", err)
			}

			all := strings.Join(helmCalls(runner), "\n")
			for _, want := range tt.want {
				if !strings.Contains(all, want) {
					t.Errorf("calls = %q, want %q", all, want)
				}
			}
			if tt.tracing.Endpoint == "" && strings.Contains(all, "tracing.") {
				t.Errorf("calls = %q, want no tracing values", all)
			}
			if strings.Contains(all, token) || strings.Contains(all, encoded) {
				t.Errorf("calls = %q, want the headers in no argument", all)
			}
			if tt.wantEnv == nil {
				if strings.Contains(all, "OTEL_EXPORTER_OTLP_HEADERS") {
					t.Errorf("calls = %q, want no headers for the controller", all)
				}
				for _, body := range runner.extraValuesFiles[cweReleaseName] {
					if strings.Contains(body, "OTEL_EXPORTER_OTLP_HEADERS") || strings.Contains(body, "env:") {
						t.Errorf("a values file sets the controller's environment without headers:\n%s", body)
					}
				}
				return
			}
			if strings.Contains(all, "env[") {
				t.Errorf("calls = %q, want every env entry in the values file", all)
			}
			got := map[string]string{}
			for _, body := range runner.extraValuesFiles[cweReleaseName] {
				var doc struct {
					Env []struct{ Name, Value string } `yaml:"env"`
				}
				if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
					t.Fatalf("a values file is not YAML: %v", err)
				}
				for _, e := range doc.Env {
					got[e.Name] = e.Value
				}
			}
			if !maps.Equal(got, tt.wantEnv) {
				t.Errorf("env in the values file = %v, want %v", got, tt.wantEnv)
			}
			// Every values file of the controller install is removed once Deploy returns.
			for _, call := range runner.calls {
				if call.name != "helm" || !slices.Contains(call.args, cweReleaseName) {
					continue
				}
				for i, arg := range call.args {
					if arg != "--values" || i+1 >= len(call.args) {
						continue
					}
					if _, err := os.Stat(call.args[i+1]); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("values file %s still exists after Deploy", call.args[i+1])
					}
				}
			}
		})
	}
}

func TestEnsureDefaultStorageClass(t *testing.T) {
	tests := []struct {
		name        string
		defaults    string
		wantInstall bool
	}{
		{name: "installs local-path when no StorageClass is the default", defaults: "\n\n", wantInstall: true},
		{name: "keeps a cluster's own default StorageClass", defaults: "\ntrue\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{outputs: []fakeOutput{{output: tt.defaults}, {}, {}, {}}}
			if err := New(runner, nil).EnsureDefaultStorageClass(context.Background(), "site.kubeconfig"); err != nil {
				t.Fatal(err)
			}
			if !tt.wantInstall {
				if len(runner.calls) != 1 {
					t.Fatalf("want only the StorageClass lookup, got %d calls", len(runner.calls))
				}
				return
			}
			if len(runner.applied) != 1 || !strings.Contains(runner.applied[0], "rancher/local-path-provisioner:v0.0.31") {
				t.Fatalf("want the local-path manifest applied, got %d manifests", len(runner.applied))
			}
			want := "annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite"
			if got := joinArgs(runner.calls[2].args[2:]); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			want = "-n local-path-storage rollout status deployment/local-path-provisioner --timeout 5m0s"
			if got := joinArgs(runner.calls[3].args[2:]); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}
