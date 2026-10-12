// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

var (
	nico    = filepath.Join("..", "..", "..", "deploy", "nico")
	example = filepath.Join("..", "..", "examples", "sitespec.yaml")
)

func TestItemsAndReleases(t *testing.T) {
	items, err := Items(nico)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 11 || items[0].Name != "nvke-base-config" || items[0].Type != TypeTemplate || items[10].Name != "controller" {
		t.Errorf("items: %+v", items)
	}
	controller, err := ReadRelease(filepath.Join(nico, "controller"))
	if err != nil {
		t.Fatal(err)
	}
	if controller.Chart.Name != "nke-controller" || controller.Chart.Path == "" || controller.Release.Namespace != "nke-controller" {
		t.Errorf("controller: %+v", controller.Chart)
	}
	spire, err := ReadRelease(filepath.Join(nico, "spire"))
	if err != nil {
		t.Fatal(err)
	}
	if len(spire.Requires) != 1 || spire.Requires[0].Kind != "certificate" {
		t.Errorf("spire requires: %+v", spire.Requires)
	}
	// Marshal writes what the shell-side reader accepts, and round-trips.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "release.yaml"), spire.Marshal(), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := ReadRelease(dir)
	if err != nil {
		t.Fatalf("round trip: %v\n%s", err, spire.Marshal())
	}
	if again.Chart.Version != spire.Chart.Version || len(again.Values) != len(spire.Values) || again.Requires[0] != spire.Requires[0] {
		t.Errorf("round trip changed the release:\n%s", spire.Marshal())
	}
	if !strings.Contains(string(spire.Marshal()), "  version: \"0.30.1\"\n") || !strings.Contains(string(spire.Marshal()), "  - {kind: certificate, name: oidc-provider-upstream-ca, namespace: spire-server, condition: Ready}\n") {
		t.Errorf("marshal grammar:\n%s", spire.Marshal())
	}
}

func TestReadRelease_Problems(t *testing.T) {
	cases := map[string]string{
		"chart:\n  name: x\n": "chart.name and chart.version are required",
		"chart:\n  name: x\n  version: \"1\"\n  path: a\n  repository: b\n":     "both repository and path",
		"chart:\n  name: x\n  version: \"1\"\n  path: a\nrelease:\n  name: r\n": "release.name and release.namespace",
		"chart:\n  name: x\n  version: \"1\"\n  colour: red\n":                  "field colour not found",
		"image: ghcr.io/x/y\n": "image must be <repository>:<tag>",
		"image: ghcr.io/x/y@sha256:" + strings.Repeat("a", 64) + "\n": "with no digest",
	}
	for text, want := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "release.yaml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadRelease(dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", text, want, err)
		}
	}
}

// filledSiteSpec writes the example site spec with every controllerStack
// placeholder filled in and the secret files it names, plus a SPIRE CA when
// withCA is set, and loads it.
func filledSiteSpec(t *testing.T, withCA bool, extra map[string]string) *sitespec.SiteSpec {
	t.Helper()
	dir := t.TempDir()
	text, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"nico-token": "nico-secret", "registry-token": "registry-secret", "tenant-password": "tenant-secret", "ca.crt": "CERT", "ca.key": "KEY", "db-password": "db-secret"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values := map[string]string{
		"REPLACE_ARCH": "arm64", "REPLACE_NICO_ENDPOINT": "https://nico.example.com", "REPLACE_NICO_ORG_ID": "org-1",
		"REPLACE_NICO_API_NAME": "forge", "REPLACE_NICO_TOKEN_PATH": "nico-token",
		"REPLACE_REGISTRY_SERVER": "ghcr.io", "REPLACE_REGISTRY_USERNAME": "robot", "REPLACE_REGISTRY_TOKEN_PATH": "registry-token",
		"REPLACE_TENANT_USER": "nvke", "REPLACE_TENANT_PASSWORD_PATH": "tenant-password",
	}
	for k, v := range extra {
		values[k] = v
	}
	body := string(text)
	for placeholder, value := range values {
		body = strings.ReplaceAll(body, placeholder, value)
	}
	if withCA {
		// The bootstrap resolves the CA paths from the working directory, so
		// they are absolute here.
		body += "spire:\n  caCertFile: " + filepath.Join(dir, "ca.crt") + "\n  caKeyFile: " + filepath.Join(dir, "ca.key") + "\n"
	}
	path := filepath.Join(dir, "sitespec.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := sitespec.Load(path)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, body)
	}
	return spec
}

func readValues(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[p]
	}
	return cur
}

func TestRender_FromSiteSpec(t *testing.T) {
	spec := filledSiteSpec(t, true, nil)
	out := filepath.Join(t.TempDir(), "rendered")
	result, err := Render(RenderOptions{DeployPath: nico, Site: spec, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("rendered tree is not private: %v %v", info.Mode(), err)
	}
	// Nothing was written into the committed deploy path.
	for _, f := range []string{"controller/site-values.yaml", "site.env", "db-password"} {
		if _, err := os.Stat(filepath.Join(nico, f)); err == nil {
			t.Errorf("the committed deploy path gained %s", f)
		}
	}
	vip := spec.Cluster.ControlPlaneVIP
	td := spec.ClusterName + ".nke.local"
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	if dig(ctl, "spiffe", "trustDomain") != td || dig(ctl, "spiffe", "spire", "serverAddress") != vip ||
		dig(ctl, "nkeOIDCAuth", "discoveryURL") != "https://"+vip+":30443/.well-known/openid-configuration" {
		t.Errorf("controller values: %v", ctl)
	}
	if _, has := ctl["image"]; has {
		t.Error("controller image set without an override")
	}
	password, _ := dig(ctl, "appDb", "postgresPassword").(string)
	stored, _ := os.ReadFile(filepath.Join(out, DBPasswordFile))
	if password == "" || strings.TrimSpace(string(stored)) != password {
		t.Errorf("generated password %q is not kept in %s (%q)", password, DBPasswordFile, stored)
	}
	pg := readValues(t, filepath.Join(out, "postgresql", SiteValuesFile))
	tp := readValues(t, filepath.Join(out, "temporal", SiteValuesFile))
	if dig(pg, "auth", "postgresPassword") != password || dig(pg, "auth", "password") != password ||
		dig(tp, "server", "config", "persistence", "default", "sql", "password") != password {
		t.Errorf("the database password differs between items")
	}
	sp := readValues(t, filepath.Join(out, "spire", SiteValuesFile))
	if dig(sp, "global", "spire", "trustDomain") != td || dig(sp, "global", "spire", "clusterName") != spec.ClusterName {
		t.Errorf("spire identity: %v", sp)
	}
	if ids, _ := dig(sp, "spire-server", "adminIDs").([]any); len(ids) != 1 || ids[0] != "spiffe://"+td+"/nke-service/nke-controller" {
		t.Errorf("adminIDs: %v", dig(sp, "spire-server", "adminIDs"))
	}
	if dig(sp, "spire-server", "nodeAttestor", "tpmDirect", "checksum") != "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16" {
		t.Errorf("arm64 TPM checksum: %v", dig(sp, "spire-server", "nodeAttestor", "tpmDirect", "checksum"))
	}
	if dig(sp, "spire-server", "upstreamAuthority", "disk", "secret", "data", "certificate") != "CERT" || dig(sp, "spire-server", "upstreamAuthority", "disk", "secret", "data", "bundle") != "CERT" {
		t.Errorf("upstream authority: %v", dig(sp, "spire-server", "upstreamAuthority"))
	}
	domains, _ := dig(sp, "spiffe-oidc-discovery-provider", "config", "additionalDomains").([]any)
	if len(domains) != 4 || domains[0] != "nke.site-controller.internal" || domains[3] != vip {
		t.Errorf("additionalDomains: %v", domains)
	}
	// Each values file is appended to the item's list, after the committed
	// ones, and a committed chart path is absolute so the tree installs from
	// anywhere.
	rel, _ := ReadRelease(filepath.Join(out, "controller"))
	if rel.Values[len(rel.Values)-1] != SiteValuesFile || rel.Values[0] != "values.yaml" {
		t.Errorf("controller values list: %v", rel.Values)
	}
	if !filepath.IsAbs(rel.Chart.Path) {
		t.Errorf("controller chart path is not absolute: %s", rel.Chart.Path)
	}
	if info, err := os.Stat(rel.Chart.Path); err != nil || !info.IsDir() {
		t.Errorf("controller chart path does not exist: %s (%v)", rel.Chart.Path, err)
	}
	for _, item := range []string{"postgresql", "spire-crds"} {
		if r, _ := ReadRelease(filepath.Join(out, item)); r.Chart.Path != "" {
			t.Errorf("%s gained a chart path: %q", item, r.Chart.Path)
		}
	}
	// Template items are rendered and become manifests.
	secret, err := os.ReadFile(filepath.Join(out, "nvke-base-config", "20-nico-credentials.yaml"))
	if err != nil || !strings.Contains(string(secret), `token: "nico-secret"`) || !strings.Contains(string(secret), `endpoint: "https://nico.example.com"`) {
		t.Errorf("rendered nico credentials: %v\n%s", err, secret)
	}
	if entries, _ := filepath.Glob(filepath.Join(out, "nvke-base-config", "*.tmpl")); len(entries) != 0 {
		t.Errorf("templates left in the rendered item: %v", entries)
	}
	items, err := Items(out)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Name != "nvke-base-config" || items[0].Type != TypeManifest || items[len(items)-1].Name != "controller" {
		t.Errorf("rendered item list: %+v", items)
	}
	env, _ := os.ReadFile(filepath.Join(out, "site.env"))
	if string(env) != "SITE_NAME="+spec.ClusterName+"\nSITE_VIP="+vip+"\nSITE_TRUST_DOMAIN="+td+"\n" {
		t.Errorf("site.env:\n%s", env)
	}
	if len(result.Notes) == 0 {
		t.Error("no notes")
	}
}

func TestRender_SiteSpecOptions(t *testing.T) {
	// No CA, a configured database password and an nktp values file.
	spec := filledSiteSpec(t, false, nil)
	dir := filepath.Dir(spec.ResolvePath("x"))
	if err := os.WriteFile(filepath.Join(dir, "nktp.yaml"), []byte("gateway:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.ControllerStack.Database.PasswordFile = "db-password"
	spec.ControllerStack.NktpValuesFile = "nktp.yaml"
	spec.ControllerStack.Mirror = "mirror.example.test:5000"
	spec.ControllerStack.Images.Controller = "mirror.example.test:5000/cwe@sha256:" + strings.Repeat("a", 64)
	out := filepath.Join(t.TempDir(), "rendered")
	if _, err := Render(RenderOptions{DeployPath: nico, Site: spec, Out: out}); err != nil {
		t.Fatal(err)
	}
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	if dig(ctl, "appDb", "postgresPassword") != "db-secret" || dig(ctl, "image", "digest") != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("controller values: %v", ctl)
	}
	if _, err := os.Stat(filepath.Join(out, DBPasswordFile)); err == nil {
		t.Error("a configured password was written to db-password")
	}
	sp := readValues(t, filepath.Join(out, "spire", SiteValuesFile))
	if _, has := sp["spire-server"].(map[string]any)["upstreamAuthority"]; has {
		t.Error("upstream authority without CA files")
	}
	if dig(sp, "global", "spire", "image", "registry") != "mirror.example.test:5000" {
		t.Errorf("spire mirror: %v", sp)
	}
	pg := readValues(t, filepath.Join(out, "postgresql", SiteValuesFile))
	if dig(pg, "global", "imageRegistry") != "mirror.example.test:5000" || dig(pg, "global", "security", "allowInsecureImages") != true {
		t.Errorf("postgresql mirror: %v", pg)
	}
	tp := readValues(t, filepath.Join(out, "temporal", SiteValuesFile))
	if dig(tp, "server", "image", "repository") != "mirror.example.test:5000/temporalio/server" {
		t.Errorf("temporal mirror: %v", tp)
	}
	nktp, _ := os.ReadFile(filepath.Join(out, "nktp", SiteValuesFile))
	if string(nktp) != "gateway:\n  enabled: true\n" {
		t.Errorf("nktp passthrough: %q", nktp)
	}
}

func TestRender_SiteSpecRefusals(t *testing.T) {
	// Placeholders left, every problem at once, nothing left behind.
	spec := filledSiteSpec(t, false, map[string]string{"REPLACE_ARCH": "REPLACE_ARCH", "REPLACE_TENANT_USER": "REPLACE_TENANT_USER"})
	out := filepath.Join(t.TempDir(), "rendered")
	_, err := Render(RenderOptions{DeployPath: nico, Site: spec, Out: out})
	if err == nil || !strings.Contains(err.Error(), "controllerStack.arch is still a REPLACE_* placeholder") || !strings.Contains(err.Error(), "controllerStack.tenant.user is still") {
		t.Errorf("placeholders: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused render left its output behind")
	}
	// A secret file that is missing.
	spec = filledSiteSpec(t, false, map[string]string{"REPLACE_NICO_TOKEN_PATH": "/nonexistent/token"})
	_, err = Render(RenderOptions{DeployPath: nico, Site: spec, Out: out})
	if err == nil || !strings.Contains(err.Error(), "controllerStack.nico.tokenFile") {
		t.Errorf("missing token file: %v", err)
	}
	// --out must not exist, and an existing one is left alone.
	if err := os.MkdirAll(filepath.Join(out, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(RenderOptions{DeployPath: nico, Site: filledSiteSpec(t, false, nil), Out: out}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("existing out accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "keep")); err != nil {
		t.Error("an existing --out was removed")
	}
	// --out inside the deploy path is refused before anything is copied. A
	// copy of the deploy path, so a failed guard cannot write into the checkout.
	copied := filepath.Join(t.TempDir(), "deploy", "nico")
	if err := copyTree(nico, copied); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(copied, "rendered-inside")
	if _, err := Render(RenderOptions{DeployPath: copied, Site: filledSiteSpec(t, false, nil), Out: inside}); err == nil || !strings.Contains(err.Error(), "inside the deploy path") {
		t.Errorf("nested out accepted: %v", err)
	}
	if _, statErr := os.Stat(inside); statErr == nil {
		t.Error("nested out was created")
	}
	// A secret the templates cannot carry is refused by field.
	for name, content := range map[string]string{"quote": "tok\"en", "newline": "abc\ndef", "backslash": "a\\b"} {
		spec := filledSiteSpec(t, false, nil)
		if err := os.WriteFile(spec.ResolvePath("tenant-password"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Render(RenderOptions{DeployPath: nico, Site: spec, Out: filepath.Join(t.TempDir(), "r")})
		if err == nil || !strings.Contains(err.Error(), "controllerStack.tenant.passwordFile: the value holds a character") {
			t.Errorf("%s in a password: %v", name, err)
		}
	}
	// A checkout under a path with whitespace cannot be carried by the reader.
	// The committed chart paths climb two levels, so the space sits above that.
	spaced := filepath.Join(t.TempDir(), "sp ace", "repo", "deploy", "nico")
	if err := copyTree(nico, spaced); err != nil {
		t.Fatal(err)
	}
	_, err = Render(RenderOptions{DeployPath: spaced, Site: filledSiteSpec(t, false, nil), Out: filepath.Join(t.TempDir(), "r")})
	if err == nil || !strings.Contains(err.Error(), "holds whitespace") {
		t.Errorf("deploy path with a space: %v", err)
	}
	// An item type the executor does not dispatch is refused when read.
	typed := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, typed); err != nil {
		t.Fatal(err)
	}
	typedList, _ := os.ReadFile(filepath.Join(typed, "nvke-deploy.yaml"))
	if err := os.WriteFile(filepath.Join(typed, "nvke-deploy.yaml"), []byte(strings.Replace(string(typedList), "type: helm}", "type: foo.bar}", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Items(typed); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("unknown item type: %v", err)
	}
	// nktpValuesFile without an nktp item is a problem, not a silent skip.
	spec = filledSiteSpec(t, false, nil)
	spec.ControllerStack.NktpValuesFile = "nktp.yaml"
	bare := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, bare); err != nil {
		t.Fatal(err)
	}
	list, _ := os.ReadFile(filepath.Join(bare, "nvke-deploy.yaml"))
	withoutNktp := strings.ReplaceAll(string(list), "  - {name: nktp, type: helm}\n", "")
	if withoutNktp == string(list) {
		t.Fatal("nktp item not found in the deploy path")
	}
	if err := os.WriteFile(filepath.Join(bare, "nvke-deploy.yaml"), []byte(withoutNktp), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.ResolvePath("nktp.yaml"), []byte("a: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Render(RenderOptions{DeployPath: bare, Site: spec, Out: filepath.Join(t.TempDir(), "r")})
	if err == nil || !strings.Contains(err.Error(), "no nktp helm item") {
		t.Errorf("nktp values without the item: %v", err)
	}
}

func TestImageValuesAndOIDCDomains(t *testing.T) {
	if v := imageValues("r.example/x:v1"); v["repository"] != "r.example/x" || v["tag"] != "v1" {
		t.Errorf("tag: %v", v)
	}
	if v := imageValues("r.example/x:v1@sha256:" + strings.Repeat("a", 64)); v["repository"] != "r.example/x" || v["tag"] != "" || v["digest"] == nil {
		t.Errorf("tag beside a digest is dropped: %v", v)
	}
	if v := imageValues("r.example:5000/x"); v["repository"] != "r.example:5000/x" || v["tag"] != nil {
		t.Errorf("port without tag: %v", v)
	}
	domains, err := oidcDomains(filepath.Join(nico, "spire", "overlay.yaml"), "10.0.0.100")
	if err != nil || len(domains) != 4 || domains[3] != "10.0.0.100" {
		t.Errorf("domains: %v %v", domains, err)
	}
	// Comments and blank lines inside the list do not truncate it.
	overlay := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(overlay, []byte("spiffe-oidc-discovery-provider:\n  config:\n    additionalDomains:\n      - a.internal\n      # a comment\n\n      - b.internal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if domains, err := oidcDomains(overlay, "1.2.3.4"); err != nil || len(domains) != 3 || domains[1] != "b.internal" {
		t.Errorf("list with a comment: %v %v", domains, err)
	}
	if _, err := oidcDomains(filepath.Join(nico, "nvke-deploy.yaml"), "10.0.0.100"); err == nil {
		t.Error("a file without the block was accepted")
	}
}
