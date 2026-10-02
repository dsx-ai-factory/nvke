// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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
	Kubeconfig:  kubeconfigPath,
	Assets:      assetsDir,
}

func testSBOM() SBOM {
	return SBOM{
		Controller: Image{Repository: "nvcr.io/nke/cwe", Tag: "v1.2.3"},
		Profiles:   Image{Repository: "nvcr.io/nke/profiles", Tag: "run-42"},
		Nico:       Nico{Endpoint: "https://forge.example", OrgID: "org", Token: "tok"},
		Registry:   Registry{Server: "ghcr.io", Username: "u", Token: "r"},
		Tenant:     Tenant{User: "admin", Password: "p"},
		Arch:       "arm64",
	}
}

func overlayPath() string {
	return path.Join(assetsDir, "controller", "deploy", "nke-sbom", "spire", "overlay.yaml")
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

	controller := decode(t, files["controller.yaml"])
	for keys, want := range map[string]string{
		"image.repository":                 "nvcr.io/nke/cwe",
		"image.tag":                        "v1.2.3",
		"appDb.postgresPassword":           "db-secret",
		"nkeOIDCAuth.discoveryURL":         "https://10.0.0.5:30443/.well-known/openid-configuration",
		"spiffe.trustDomain":               "e2e-site.nke.local",
		"spiffe.spire.serverAddress":       "10.0.0.5",
		"nodeAgent.spireAttestationMethod": "tpm",
		"profiles.image.repository":        "nvcr.io/nke/profiles",
		"profiles.image.tag":               "run-42",
	} {
		if got := dig(t, controller, strings.Split(keys, ".")...); got != want {
			t.Errorf("controller %s = %v, want %s", keys, got, want)
		}
	}
	env := mustJSON(t, controller["env"])
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
	if got := mustJSON(t, dig(t, spire, "spire-server", "adminIDs")); got != `["spiffe://e2e-site.nke.local/nke-service/nke-controller"]` {
		t.Errorf("adminIDs = %s", got)
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
		Registry    map[string]string `json:"Registry"`
		Tenant      map[string]string `json:"Tenant"`
		ClusterName string            `json:"ClusterName"`
	}
	if err := json.Unmarshal(files["bootstrap-config.json"], &config); err != nil {
		t.Fatal(err)
	}
	if config.Nico["APIName"] != "forge" || config.Nico["Token"] != "tok" || config.Registry["Server"] != "ghcr.io" ||
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
		"node-role.kubernetes.io/control-plane-",
		"clusterctl init",
		"local-path-storage.yaml",
		"use-nke-sbom.sh",
		"apply-capi-spiffe-exec-auth.sh",
		"rm -rf",
	}
	last := -1
	for _, want := range order {
		i := s.ran(want)
		if i <= last {
			t.Fatalf("%q ran at %d, want after %d:\n%s", want, i, last, strings.Join(s.runs, "\n"))
		}
		last = i
	}
	install := s.cmd(t, "use-nke-sbom.sh")
	for _, want := range []string{
		"--sbom /var/lib/nke-e2e/assets/controller/deploy",
		"--target nke-sbom",
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
		if strings.Contains(cmd, "use-nke-sbom.sh") {
			return "helm: release temporal failed", errors.New("exit status 1")
		}
		return "", nil
	}
	err := testSBOM().Deploy(context.Background(), s, sbomTarget)
	if err == nil || !strings.Contains(err.Error(), "install nke-sbom") || !strings.Contains(err.Error(), "release temporal failed") {
		t.Fatalf("err = %v", err)
	}
	if s.ran("apply-capi-spiffe-exec-auth.sh") >= 0 {
		t.Error("ran the post-install step after a failed install")
	}
	if s.ran("rm -rf") < 0 {
		t.Error("left the values, which hold credentials, on the node")
	}
}

func TestSBOMValidation(t *testing.T) {
	for name, mutate := range map[string]func(*SBOM){
		"arch":          func(d *SBOM) { d.Arch = "riscv64" },
		"controller":    func(d *SBOM) { d.Controller.Tag = "" },
		"profiles":      func(d *SBOM) { d.Profiles = Image{} },
		"nico":          func(d *SBOM) { d.Nico.Token = "" },
		"storage class": func(d *SBOM) { d.StorageClassManifest = "https://example/sc.yaml" },
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
