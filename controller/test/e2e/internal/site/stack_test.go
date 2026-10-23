// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

const spireOverlay = `
spiffe-oidc-discovery-provider:
  config:
    additionalDomains:
      - "nke.site-controller.internal"
      - "localhost"
      - "127.0.0.1"
`

var sbomTarget = DeployTarget{
	Node:        testNode,
	SiteName:    "e2e-site",
	TrustDomain: "e2e-site.nke.local",
	Cluster:     Cluster{NodeKubeconfig: kubeconfigPath},
}

// stackCheckout is a tree with the two paths the stack stages, written once
// for the package rather than per test: Stack.validate stats it, and nothing
// mutates it.
var stackCheckout = func() string {
	dir, err := os.MkdirTemp("", "nke-stack-assets-")
	if err != nil {
		panic(err)
	}
	for _, rel := range []string{"deploy/nvke-deploy.sh", "deploy/nico/release.yaml"} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			panic(err)
		}
	}
	return dir
}()

func testSBOM() Stack {
	layout := CurrentLayout()
	layout.Export = []string{"deploy"}
	return Stack{
		Assets:     DeployAssets{Dir: stackCheckout, Layout: layout},
		Controller: Image{Repository: "nvcr.io/nvke/cwe", Tag: "v1.2.3"},
		Profiles:   Image{Repository: "nvcr.io/nvke/profiles", Tag: "run-42"},
		Provider:   NewNico("https://forge.example", "org", "tok", "forge"),
		Tenant:     Tenant{User: "admin", Password: "p"},
		Arch:       "arm64",
	}
}

func overlayPath() string {
	return path.Join(assetsDir, "deploy", "nico", "spire", "overlay.yaml")
}

func sbomSession() *fakeSession {
	s := newFakeSession()
	s.fetch[overlayPath()] = []byte(spireOverlay)
	return s
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func dig(t *testing.T, doc any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := doc.(map[string]any)
		if !ok {
			t.Fatalf("%s: not a map at %q", strings.Join(keys, "."), k)
		}
		doc = m[k]
	}
	return doc
}

func TestSBOMRender(t *testing.T) {
	d := testSBOM().withDefaults()
	d.DatabasePassword = "db-secret"
	d.ControllerEnv = map[string]string{"DGXC_REGION": "us-west"}
	files, err := d.render(sbomTarget, []byte(spireOverlay))
	if err != nil {
		t.Fatal(err)
	}

	controllerValues := decode(t, files["controller.yaml"])
	for keys, want := range map[string]string{
		"image.repository":                 "nvcr.io/nvke/cwe",
		"image.tag":                        "v1.2.3",
		"appDb.postgresPassword":           "db-secret",
		"nvkeOIDCAuth.discoveryURL":        "https://10.0.0.5:30443/.well-known/openid-configuration",
		"spiffe.trustDomain":               "e2e-site.nke.local",
		"spiffe.spire.serverAddress":       "10.0.0.5",
		"nodeAgent.spireAttestationMethod": "tpm",
		"profiles.image.repository":        "nvcr.io/nvke/profiles",
		"profiles.image.tag":               "run-42",
	} {
		if got := dig(t, controllerValues, strings.Split(keys, ".")...); got != want {
			t.Errorf("controller %s = %v, want %s", keys, got, want)
		}
	}
	env := mustJSON(t, controllerValues["env"])
	for _, want := range []string{
		`{"name":"NODE_AGENT_PORT","value":"15005"}`,
		`{"name":"SPIRE_SERVER_API_ADDRESS","value":"spire-server.spire-server.svc.cluster.local:443"}`,
		`{"name":"DGXC_REGION","value":"us-west"}`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("controller env %s lacks %s", env, want)
		}
	}

	spire := decode(t, files["spire.yaml"])
	if got := dig(t, spire, "spire-server", "nodeAttestor", "tpmDirect", "checksum"); got != mustChecksum(t, "arm64") {
		t.Errorf("TPM checksum = %v", got)
	}
	// Follows the name the controller is installed under rather than asserting one, for
	// the reason spireAdminPath gives: the deploy path chooses it and this
	// file does not. TestSBOMAdminIDFollowsTheControllerName covers the
	// following itself.
	wantAdminIDs := fmt.Sprintf(`["spiffe://e2e-site.nke.local/nvke-service/%s"]`, controller.Service())
	if got := mustJSON(t, dig(t, spire, "spire-server", "adminIDs")); got != wantAdminIDs {
		t.Errorf("adminIDs = %s, want %s", got, wantAdminIDs)
	}
	if got := mustJSON(t, dig(t, spire, "spiffe-oidc-discovery-provider", "config", "additionalDomains")); got !=
		`["nke.site-controller.internal","localhost","127.0.0.1","10.0.0.5"]` {
		t.Errorf("additionalDomains = %s", got)
	}
	if got := dig(t, spire, "global", "spire", "clusterName"); got != "e2e-site" {
		t.Errorf("clusterName = %v", got)
	}

	// Both Temporal datastores, not just one: setting the password on only
	// one leaves the schema job crash-looping on password authentication.
	for _, want := range []struct {
		file string
		keys []string
	}{
		{"postgresql.yaml", []string{"auth", "postgresPassword"}},
		{"temporal.yaml", []string{"server", "config", "persistence", "default", "sql", "password"}},
		{"temporal.yaml", []string{"server", "config", "persistence", "visibility", "sql", "password"}},
	} {
		if got := dig(t, decode(t, files[want.file]), want.keys...); got != "db-secret" {
			t.Errorf("%s %v = %v", want.file, want.keys, got)
		}
	}

	var config struct {
		Nico        map[string]string `json:"Nico"`
		Tenant      map[string]string `json:"Tenant"`
		ClusterName string            `json:"ClusterName"`
	}
	if err := json.Unmarshal(files["bootstrap-config.json"], &config); err != nil {
		t.Fatal(err)
	}
	if config.Nico["APIName"] != "forge" || config.Nico["Token"] != "tok" ||
		config.Tenant["User"] != "admin" || config.ClusterName != "e2e-site" {
		t.Errorf("bootstrap config = %+v", config)
	}
}

func TestOIDCDomains(t *testing.T) {
	got, err := oidcDomains([]byte(spireOverlay), "127.0.0.1")
	if err != nil || len(got) != 3 {
		t.Errorf("a VIP already listed was added again: %v, %v", got, err)
	}
	if _, err := oidcDomains([]byte("spiffe-oidc-discovery-provider: {}\n"), "10.0.0.5"); err == nil {
		t.Error("an overlay without additionalDomains was accepted")
	}
}

func TestSBOMDeploy(t *testing.T) {
	s := sbomSession()
	if err := testSBOM().Deploy(context.Background(), s, sbomTarget); err != nil {
		t.Fatal(err)
	}

	valuesDir := path.Join(workDir, "values")
	for _, name := range []string{"controller.yaml", "spire.yaml", "postgresql.yaml", "temporal.yaml", "bootstrap-config.json"} {
		p, ok := s.puts[path.Join(valuesDir, name)]
		if !ok {
			t.Errorf("%s not staged", name)
			continue
		}
		if p.mode != 0o600 {
			t.Errorf("%s mode %o, want 600", name, p.mode)
		}
	}
	pg := decode(t, s.puts[path.Join(valuesDir, "postgresql.yaml")].data)
	if pw, _ := dig(t, pg, "auth", "postgresPassword").(string); len(pw) < 32 {
		t.Errorf("generated password %q is too short", pw)
	}

	order := []string{
		// The deploy path is staged first: every step after it runs from the
		// tree it unpacks.
		"assets.tar.gz",
		"node-role.kubernetes.io/control-plane-",
		"clusterctl init",
		"local-path-storage.yaml",
		"nvke-deploy.sh",
		"apply-capi-spiffe-exec-auth.sh",
	}
	last := -1
	for _, want := range order {
		i := s.ran(want)
		if i <= last {
			t.Fatalf("%q ran at %d, want after %d:\n%s", want, i, last, strings.Join(s.runs, "\n"))
		}
		last = i
	}
	// The rendered values hold credentials, so removing them is the last thing
	// that happens. Asserted on the final command rather than by searching for
	// "rm -rf", which the staging step also runs.
	if final := s.runs[len(s.runs)-1]; !strings.Contains(final, valuesDir) || !strings.Contains(final, "rm -rf") {
		t.Errorf("last command should remove the values directory, got %s", final)
	}
	install := s.cmd(t, "nvke-deploy.sh")
	for _, want := range []string{
		"nvke-deploy.sh install ",
		"--deploy-path /var/lib/nke-e2e/assets/deploy/nico",
		"--config",
		"controller=/var/lib/nke-e2e/values/controller.yaml",
		"temporal=/var/lib/nke-e2e/values/temporal.yaml",
		"--kubeconfig",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("install lacks %s: %s", want, install)
		}
	}
	for _, cmd := range s.runs {
		if strings.Contains(cmd, "tok") {
			t.Errorf("a credential reached a command line: %s", cmd)
		}
	}
}

func TestSBOMDeployCleansUpOnFailure(t *testing.T) {
	s := sbomSession()
	s.fail = func(cmd string) (string, error) {
		if strings.Contains(cmd, "nvke-deploy.sh") {
			return "helm: release temporal failed", errors.New("exit status 1")
		}
		return "", nil
	}
	err := testSBOM().Deploy(context.Background(), s, sbomTarget)
	if err == nil || !strings.Contains(err.Error(), "install deploy/nico") || !strings.Contains(err.Error(), "release temporal failed") {
		t.Fatalf("err = %v", err)
	}
	if s.ran("apply-capi-spiffe-exec-auth.sh") >= 0 {
		t.Error("ran the post-install step after a failed install")
	}
	if s.ran("rm -rf") < 0 {
		t.Error("left the values, which hold credentials, on the node")
	}
}

func TestStackValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Stack){
		"arch":          func(d *Stack) { d.Arch = "riscv64" },
		"controller":    func(d *Stack) { d.Controller.Tag = "" },
		"profiles":      func(d *Stack) { d.Profiles = Image{} },
		"provider":      func(d *Stack) { d.Provider = NewNico("https://forge.example", "org", "", "forge") },
		"no provider":   func(d *Stack) { d.Provider = nil },
		"storage class": func(d *Stack) { d.StorageClassManifest = "https://example/sc.yaml" },
	} {
		t.Run(name, func(t *testing.T) {
			d := testSBOM()
			mutate(&d)
			s := sbomSession()
			if err := d.Deploy(context.Background(), s, sbomTarget); err == nil {
				t.Fatal("expected a validation error")
			}
			if len(s.runs) != 0 || len(s.puts) != 0 {
				t.Error("touched the node before validating")
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustChecksum(t *testing.T, arch string) string {
	t.Helper()
	sum, ok := tpmServerChecksum(arch)
	if !ok {
		t.Fatalf("no checksum for %s", arch)
	}
	return sum
}

// assetsInstalling is a deploy path whose controller HelmRelease installs
// under name, which is all CheckControllerName reads.
func assetsInstalling(t *testing.T, deployPath, name string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ControllerReleasePath(deployPath)),
		"chart:\n  name: nvke-controller\nrelease:\n  name: c\n  namespace: "+name+"\n")
	return dir
}

// TestCheckControllerName covers the guard that runs before a machine is
// taken: a deploy path installing the controller under a name the suite does
// not look for fails on the runner, naming both and the variables to set.
func TestCheckControllerName(t *testing.T) {
	deployPath := CurrentLayout().DeployPath
	for _, env := range []string{
		controller.EnvNamespace, controller.PreviousEnvNamespace, controller.LegacyEnvNamespace,
		controller.EnvService, controller.PreviousEnvService, controller.LegacyEnvService,
	} {
		t.Setenv(env, "")
	}

	t.Run("the suite default matches", func(t *testing.T) {
		if err := CheckControllerName(assetsInstalling(t, deployPath, controller.ServiceNamespace), deployPath); err != nil {
			t.Fatalf("a matching name was rejected: %v", err)
		}
	})

	t.Run("a different name fails", func(t *testing.T) {
		const deployed = "other-controller"
		err := CheckControllerName(assetsInstalling(t, deployPath, deployed), deployPath)
		if err == nil {
			t.Fatal("a deploy path installing a different name was accepted")
		}
		for _, want := range []string{deployed, controller.ServiceNamespace, controller.EnvNamespace, controller.EnvService} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %s: %v", want, err)
			}
		}
	})
}

// The diagnostics archive is uploaded whatever the run did, so every
// credential the provider configuration puts on the node has to be nameable
// for redaction. A DSX run's is a minted SSA bearer token, which is live for
// fifteen minutes after the archive is published.
func TestNicoNamesItsTokenAsACredential(t *testing.T) {
	t.Run("the token is named", func(t *testing.T) {
		got := NewNico("https://carbide-rest-api-dh.example", "ncx", "ssa-bearer-token", "nico").Credentials()
		if len(got) != 1 || got[0] != "ssa-bearer-token" {
			t.Errorf("Credentials() = %v, want just the token", got)
		}
	})

	t.Run("nothing else is", func(t *testing.T) {
		for _, c := range NewNico("https://carbide-rest-api-dh.example", "ncx", "tok", "nico").Credentials() {
			if strings.Contains(c, "carbide-rest-api-dh.example") || c == "ncx" || c == "nico" {
				t.Errorf("Credentials() names %q, which is infrastructure rather than a secret", c)
			}
		}
	})

	t.Run("an empty token names nothing", func(t *testing.T) {
		// Redacting the empty string would rewrite every character of the
		// archive, so a configuration without a token names none.
		if got := NewNico("https://forge.example", "org", "", "forge").Credentials(); len(got) != 0 {
			t.Errorf("Credentials() = %v, want none", got)
		}
	})
}
