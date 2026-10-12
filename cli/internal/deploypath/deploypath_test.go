// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"fmt"
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
	// Marshal writes what the shell-side reader accepts, and round-trips,
	// chart.setFile included.
	spire.Chart.SetFile = []string{"spire-server.upstreamAuthority.disk.secret.data.key=/etc/nvke/ca.key"}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "release.yaml"), spire.Marshal(), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := ReadRelease(dir)
	if err != nil {
		t.Fatalf("round trip: %v\n%s", err, spire.Marshal())
	}
	if again.Chart.Version != spire.Chart.Version || len(again.Values) != len(spire.Values) || again.Requires[0] != spire.Requires[0] ||
		len(again.Chart.SetFile) != 1 || again.Chart.SetFile[0] != spire.Chart.SetFile[0] {
		t.Errorf("round trip changed the release:\n%s", spire.Marshal())
	}
	if !strings.Contains(string(spire.Marshal()), "  setFile:\n    - spire-server.upstreamAuthority.disk.secret.data.key=/etc/nvke/ca.key\n") ||
		!strings.Contains(string(spire.Marshal()), "  version: \"0.30.1\"\n") || !strings.Contains(string(spire.Marshal()), "  - {kind: certificate, name: oidc-provider-upstream-ca, namespace: spire-server, condition: Ready}\n") {
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
		"image: ghcr.io/x/y@sha256:" + strings.Repeat("a", 64) + "\n":                                                       "with no digest",
		"chart:\n  name: x\n  version: \"1\"\n  setFile:\n    - a.b=relative/ca.crt\nrelease:\n  name: r\n  namespace: n\n": "setFile entry must be <helm key>=<absolute path>",
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
		"REPLACE_TENANT_USER": "nvke", "REPLACE_TENANT_PASSWORD_PATH": "tenant-password", "REPLACE_DB_PASSWORD_PATH": "db-password",
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
	// deploy/nico enables profiles.image without a pin, so the spec names it.
	spec.ControllerStack.Images.Profiles = "ghcr.io/example/controller-profiles:v1"
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

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		t.Fatalf("glob %s: %v %v", pattern, matches, err)
	}
	return matches
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
	result, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("rendered tree is not private: %v %v", info.Mode(), err)
	}
	// Nothing was written into the committed deploy path.
	for _, f := range []string{"controller/site-values.yaml", "site.env"} {
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
	if dig(ctl, "profiles", "image", "tag") != "v1" || dig(ctl, "profiles", "image", "digest") != "" {
		t.Errorf("profiles image: %v", ctl["profiles"])
	}
	password, _ := dig(ctl, "appDb", "postgresPassword").(string)
	if password != "db-secret" {
		t.Errorf("database password not read from the site spec's file: %q", password)
	}
	pg := readValues(t, filepath.Join(out, "postgresql", SiteValuesFile))
	tp := readValues(t, filepath.Join(out, "temporal", SiteValuesFile))
	if dig(pg, "auth", "postgresPassword") != password || dig(pg, "auth", "password") != password ||
		dig(tp, "server", "config", "persistence", "default", "sql", "password") != password {
		t.Errorf("the database password differs between items")
	}
	if _, has := pg["global"]; has {
		t.Error("postgresql mirror keys without a mirror")
	}
	if dig(tp, "server", "image") != nil || tp["web"] != nil {
		t.Error("temporal mirror keys without a mirror")
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
	// The CA is referenced, not copied: enabled in the values, the files in
	// chart.setFile.
	if dig(sp, "spire-server", "upstreamAuthority", "disk", "enabled") != true || dig(sp, "spire-server", "upstreamAuthority", "disk", "secret") != nil {
		t.Errorf("upstream authority: %v", dig(sp, "spire-server", "upstreamAuthority"))
	}
	spireRel, _ := ReadRelease(filepath.Join(out, "spire"))
	caPrefix := "spire-server.upstreamAuthority.disk.secret.data."
	if want := []string{caPrefix + "certificate=" + spec.Spire.CACertFile, caPrefix + "key=" + spec.Spire.CAKeyFile, caPrefix + "bundle=" + spec.Spire.CACertFile}; strings.Join(spireRel.Chart.SetFile, " ") != strings.Join(want, " ") {
		t.Errorf("spire setFile: %v", spireRel.Chart.SetFile)
	}
	if raw, _ := os.ReadFile(filepath.Join(out, "spire", SiteValuesFile)); strings.Contains(string(raw), "CERT") || strings.Contains(string(raw), "KEY") {
		t.Errorf("spire site values carry the CA:\n%s", raw)
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
	// The credential list names the values files that hold a secret and the
	// rendered Secret manifests; spire's values hold none. The tree ignores
	// itself for git.
	want := []string{"controller/site-values.yaml", "postgresql/site-values.yaml", "temporal/site-values.yaml"}
	for _, f := range []string{"20-nico-credentials.yaml", "30-ghcr-imagepull.yaml", "35-dgxc-tenant-credentials.yaml"} {
		want = append(want, "nvke-base-config/"+f)
	}
	if strings.Join(result.Credentials, " ") != strings.Join(want, " ") {
		t.Errorf("credentials:\n got %v\nwant %v", result.Credentials, want)
	}
	for _, f := range result.Credentials {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("credential file missing: %s", f)
		}
	}
	if ignore, err := os.ReadFile(filepath.Join(out, ".gitignore")); err != nil || string(ignore) != "*\n" {
		t.Errorf(".gitignore: %q %v", ignore, err)
	}
}

func TestRender_SiteSpecOptions(t *testing.T) {
	// No CA, a mirror, an image override and an nktp values file.
	spec := filledSiteSpec(t, false, nil)
	dir := filepath.Dir(spec.ResolvePath("x"))
	if err := os.WriteFile(filepath.Join(dir, "nktp.yaml"), []byte("gateway:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.ControllerStack.NktpValuesFile = "nktp.yaml"
	spec.ControllerStack.Mirror = "mirror.example.test:5000"
	spec.ControllerStack.Images.Controller = "mirror.example.test:5000/cwe@sha256:" + strings.Repeat("a", 64)
	out := filepath.Join(t.TempDir(), "rendered")
	if _, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out}); err != nil {
		t.Fatal(err)
	}
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	if dig(ctl, "appDb", "postgresPassword") != "db-secret" || dig(ctl, "image", "digest") != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("controller values: %v", ctl)
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
	if dig(tp, "server", "image", "repository") != "mirror.example.test:5000/temporalio/server" ||
		dig(tp, "admintools", "image", "repository") != "mirror.example.test:5000/temporalio/admin-tools" ||
		dig(tp, "web", "image", "repository") != "mirror.example.test:5000/temporalio/ui" {
		t.Errorf("temporal mirror: %v", tp)
	}
	if dig(tp, "server", "config", "persistence", "default", "sql", "password") != "db-secret" ||
		dig(tp, "server", "config", "persistence", "visibility", "sql", "password") != "db-secret" {
		t.Errorf("the mirror dropped temporal's passwords: %v", tp["server"])
	}
	nktp, _ := os.ReadFile(filepath.Join(out, "nktp", SiteValuesFile))
	if string(nktp) != "gateway:\n  enabled: true\n" {
		t.Errorf("nktp passthrough: %q", nktp)
	}
}

func TestRender_DatabasePassword(t *testing.T) {
	// A missing password file is created, private, and the one password
	// lands in every item that needs it.
	spec := filledSiteSpec(t, false, map[string]string{"REPLACE_DB_PASSWORD_PATH": "state/db-password"})
	path := spec.ResolvePath("state/db-password")
	out := filepath.Join(t.TempDir(), "rendered")
	result, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("generated password file: %v %v", info, err)
	}
	stored, _ := os.ReadFile(path)
	password := strings.TrimSpace(string(stored))
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	pg := readValues(t, filepath.Join(out, "postgresql", SiteValuesFile))
	tp := readValues(t, filepath.Join(out, "temporal", SiteValuesFile))
	if password == "" || dig(ctl, "appDb", "postgresPassword") != password || dig(pg, "auth", "password") != password ||
		dig(tp, "server", "config", "persistence", "visibility", "sql", "password") != password {
		t.Errorf("generated password %q is not the one in the values", password)
	}
	if !contains(result.Notes, "database password generated and written to "+path) {
		t.Errorf("notes: %v", result.Notes)
	}
	if _, err := os.Stat(filepath.Join(out, "db-password")); err == nil {
		t.Error("the rendered tree holds a password copy")
	}
	// Without the field the render refuses and leaves nothing.
	spec = filledSiteSpec(t, false, nil)
	spec.ControllerStack.Database.PasswordFile = ""
	out = filepath.Join(t.TempDir(), "rendered")
	_, err = Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out})
	if err == nil || !strings.Contains(err.Error(), "controllerStack.database.passwordFile is required") {
		t.Errorf("unset password file: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused render left its output behind")
	}
}

func TestRender_RefusesSharedTemp(t *testing.T) {
	spec := filledSiteSpec(t, false, nil)
	shared := filepath.Join("/tmp", fmt.Sprintf("nvke-render-refused-%d", os.Getpid()))
	for _, out := range []string{shared, filepath.Join(t.TempDir(), "rendered")} {
		_, err := Render(RenderOptions{DeployPath: nico, Site: spec, Out: out})
		if err == nil || !strings.Contains(err.Error(), "is under a shared temp directory") {
			t.Errorf("%s: %v", out, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s was created", out)
		}
	}
}

func TestRender_SiteSpecRefusals(t *testing.T) {
	// Placeholders left, every problem at once, nothing left behind.
	spec := filledSiteSpec(t, false, map[string]string{"REPLACE_ARCH": "REPLACE_ARCH", "REPLACE_TENANT_USER": "REPLACE_TENANT_USER"})
	out := filepath.Join(t.TempDir(), "rendered")
	_, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out})
	if err == nil || !strings.Contains(err.Error(), "controllerStack.arch is still a REPLACE_* placeholder") || !strings.Contains(err.Error(), "controllerStack.tenant.user is still") {
		t.Errorf("placeholders: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused render left its output behind")
	}
	// A secret file that is missing.
	spec = filledSiteSpec(t, false, map[string]string{"REPLACE_NICO_TOKEN_PATH": "/nonexistent/token"})
	_, err = Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out})
	if err == nil || !strings.Contains(err.Error(), "controllerStack.nico.tokenFile") {
		t.Errorf("missing token file: %v", err)
	}
	// --out must not exist, and an existing one is left alone.
	if err := os.MkdirAll(filepath.Join(out, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: filledSiteSpec(t, false, nil), Out: out}); err == nil || !strings.Contains(err.Error(), "exists") {
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
	if _, err := Render(RenderOptions{AllowTempOut: true, DeployPath: copied, Site: filledSiteSpec(t, false, nil), Out: inside}); err == nil || !strings.Contains(err.Error(), "inside the deploy path") {
		t.Errorf("nested out accepted: %v", err)
	}
	if _, statErr := os.Stat(inside); statErr == nil {
		t.Error("nested out was created")
	}
	// A CA path with whitespace is refused: the deploy path reader cannot carry it.
	caSpec := filledSiteSpec(t, true, nil)
	caDir := filepath.Join(t.TempDir(), "ca dir")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ca.crt", "ca.key"} {
		if err := os.WriteFile(filepath.Join(caDir, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	caSpec.Spire.CACertFile, caSpec.Spire.CAKeyFile = filepath.Join(caDir, "ca.crt"), filepath.Join(caDir, "ca.key")
	if _, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: caSpec, Out: filepath.Join(t.TempDir(), "r")}); err == nil || !strings.Contains(err.Error(), "spire.caCertFile: the path") || !strings.Contains(err.Error(), "holds whitespace") {
		t.Errorf("CA path with a space: %v", err)
	}
	// A secret the templates cannot carry is refused by field.
	for name, content := range map[string]string{"quote": "tok\"en", "newline": "abc\ndef", "backslash": "a\\b"} {
		spec := filledSiteSpec(t, false, nil)
		if err := os.WriteFile(spec.ResolvePath("tenant-password"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: filepath.Join(t.TempDir(), "r")})
		if err == nil || !strings.Contains(err.Error(), "controllerStack.tenant.passwordFile: the value holds a character") {
			t.Errorf("%s in a password: %v", name, err)
		}
	}
	// Refusals by field: no profiles image while the deploy path enables it, an
	// empty secret file, a non-secret value the templates cannot carry, an
	// image override with no pin.
	for name, tc := range map[string]struct {
		mutate func(*sitespec.SiteSpec)
		want   string
	}{
		"profiles image": {func(s *sitespec.SiteSpec) { s.ControllerStack.Images.Profiles = "" }, "controllerStack.images.profiles is required"},
		"empty secret":   {func(s *sitespec.SiteSpec) { _ = os.WriteFile(s.ResolvePath("nico-token"), []byte(" \n"), 0o600) }, "controllerStack.nico.tokenFile is empty"},
		"quoted orgID":   {func(s *sitespec.SiteSpec) { s.ControllerStack.Nico.OrgID = "org\"x" }, "controllerStack.nico.orgID: the value holds a character"},
		"unpinned image": {func(s *sitespec.SiteSpec) { s.ControllerStack.Images.Controller = "mirror.example.test/cwe" }, "controllerStack.images.controller: \"mirror.example.test/cwe\" names neither a tag nor a digest"},
	} {
		spec := filledSiteSpec(t, false, nil)
		tc.mutate(spec)
		out := filepath.Join(t.TempDir(), "r")
		_, err := Render(RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Out: out})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s: output left behind", name)
		}
	}
	// A template item with no template is refused, and `..` is not an item name.
	broken := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, broken); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range mustGlob(t, filepath.Join(broken, "nvke-base-config", "*.tmpl")) {
		if err := os.Rename(tmpl, strings.TrimSuffix(tmpl, ".tmpl")+".off"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Render(RenderOptions{AllowTempOut: true, DeployPath: broken, Site: filledSiteSpec(t, false, nil), Out: filepath.Join(t.TempDir(), "r")}); err == nil || !strings.Contains(err.Error(), "no .yaml.tmpl file") {
		t.Errorf("template item without templates: %v", err)
	}
	for _, name := range []string{"..", ".", "a..b"} {
		dotted := filepath.Join(t.TempDir(), "nico")
		if err := copyTree(nico, dotted); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dotted, "nvke-deploy.yaml"), []byte("items:\n  - {name: "+name+", type: manifest}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Items(dotted); err == nil || !strings.Contains(err.Error(), "names are tokens") {
			t.Errorf("item name %q: %v", name, err)
		}
	}
	// A checkout under a path with whitespace cannot be carried by the reader.
	// The committed chart paths climb two levels, so the space sits above that.
	spaced := filepath.Join(t.TempDir(), "sp ace", "repo", "deploy", "nico")
	if err := copyTree(nico, spaced); err != nil {
		t.Fatal(err)
	}
	_, err = Render(RenderOptions{AllowTempOut: true, DeployPath: spaced, Site: filledSiteSpec(t, false, nil), Out: filepath.Join(t.TempDir(), "r")})
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
	_, err = Render(RenderOptions{AllowTempOut: true, DeployPath: bare, Site: spec, Out: filepath.Join(t.TempDir(), "r")})
	if err == nil || !strings.Contains(err.Error(), "no nktp helm item") {
		t.Errorf("nktp values without the item: %v", err)
	}
}

func TestImageValuesAndOIDCDomains(t *testing.T) {
	if v := imageValues("r.example/x:v1"); v["repository"] != "r.example/x" || v["tag"] != "v1" || v["digest"] != "" {
		t.Errorf("tag: %v", v)
	}
	if v := imageValues("r.example/x:v1@sha256:" + strings.Repeat("a", 64)); v["repository"] != "r.example/x" || v["tag"] != "" || v["digest"] == nil {
		t.Errorf("tag beside a digest is dropped: %v", v)
	}
	if v := imageValues("r.example:5000/x"); v != nil {
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
