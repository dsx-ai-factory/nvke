// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
)

var (
	nico         = filepath.Join("..", "..", "..", "deploy", "nico")
	example      = filepath.Join("..", "..", "examples", "sitespec.yaml")
	stackExample = filepath.Join("..", "..", "..", "deploy", "examples", "stack-config.json")
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
	if controller.Chart.Name != "nvke-controller" || controller.Chart.Path == "" || controller.Release.Namespace != "nvke-controller" {
		t.Errorf("controller: %+v", controller.Chart)
	}
	if controller.Chart.Artifact != "controller-chart" || controller.Chart.ImageArtifact != "cwe" || controller.Chart.ProfilesArtifact != "controller-profiles" {
		t.Errorf("controller bindings: %+v", controller.Chart)
	}
	capnico, err := ReadRelease(filepath.Join(nico, "capnico"))
	if err != nil {
		t.Fatal(err)
	}
	if capnico.Artifact != "capnico-controller" || !strings.HasPrefix(capnico.Image, "ghcr.io/dsx-ai-factory/cluster-api-provider-nico/controller:") {
		t.Errorf("capnico: %+v", capnico)
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
		"chart:\n  version: \"1.0.0\"\n  repository: https://charts.example\n":  "chart.name is required",
		"chart:\n  name: x\n  repository: https://charts.example\n":             "chart.version is required for a repository chart",
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

// filledInputs writes the two inputs of a site render under one temp
// directory, in separate subdirectories so a test can tell which one a
// value came from: the example site spec under site/, with a trust domain
// and, when withCA is set, a SPIRE CA; the example stack config under
// stack/, every placeholder filled in. extra replaces further text of the
// stack config. The credentials are set in the environment. Both are loaded.
func filledInputs(t *testing.T, withCA bool, extra map[string]string) (*sitespec.SiteSpec, *stackconfig.Config) {
	t.Helper()
	root := t.TempDir()
	siteDir, stackDir := filepath.Join(root, "site"), filepath.Join(root, "stack")
	for _, dir := range []string{siteDir, stackDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(stackconfig.EnvNicoToken, "nico-secret")
	t.Setenv(stackconfig.EnvNodeUser, "nvke")
	t.Setenv(stackconfig.EnvNodePassword, "tenant-secret")
	t.Setenv(stackconfig.EnvDatabasePassword, "db-secret")
	t.Setenv(sitespec.EnvSpireCACertFile, "")
	t.Setenv(sitespec.EnvSpireCAKeyFile, "")
	spec, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	// The trust domain is the bootstrap's; set here so the render does not default it.
	body := string(spec) + "spire:\n  trustDomain: my-tier0-site.nke.local\n"
	if withCA {
		body += "  upstreamCA: true\n"
		for name, content := range map[string]string{"ca.crt": "CERT\n", "ca.key": "KEY\n"} {
			if err := os.WriteFile(filepath.Join(siteDir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv(sitespec.EnvSpireCACertFile, filepath.Join(siteDir, "ca.crt"))
		t.Setenv(sitespec.EnvSpireCAKeyFile, filepath.Join(siteDir, "ca.key"))
	}
	specPath := filepath.Join(siteDir, "sitespec.yaml")
	if err := os.WriteFile(specPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	site, err := sitespec.Load(specPath)
	if err != nil {
		t.Fatalf("load site spec: %v\n%s", err, body)
	}
	stackText, err := os.ReadFile(stackExample)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"REPLACE_NICO_ENDPOINT": "https://nico.example.com", "REPLACE_NICO_ORG_ID": "org-1", "REPLACE_NICO_API_NAME": "forge"}
	for k, v := range extra {
		values[k] = v
	}
	stackBody := string(stackText)
	for placeholder, value := range values {
		stackBody = strings.ReplaceAll(stackBody, placeholder, value)
	}
	stackPath := filepath.Join(stackDir, "stack-config.json")
	if err := os.WriteFile(stackPath, []byte(stackBody), 0o600); err != nil {
		t.Fatal(err)
	}
	stack, err := stackconfig.Load(stackPath)
	if err != nil {
		t.Fatalf("load stack config: %v\n%s", err, stackBody)
	}
	// deploy/nico enables profiles.image without a pin, so the config names it.
	stack.Images.Profiles = "ghcr.io/example/controller-profiles:v1"
	return site, stack
}

// siteRender is the RenderOptions of a site render into out.
func siteRender(spec *sitespec.SiteSpec, stack *stackconfig.Config, out string) RenderOptions {
	return RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Stack: stack, Arch: "linux/arm64", Out: out}
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

// readSecrets parses a multi-document Secret manifest into
// namespace/name -> decoded data.
func readSecrets(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	docs, err := yamlDocuments(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	for _, raw := range docs {
		var doc struct {
			Kind     string `yaml:"kind"`
			Type     string `yaml:"type"`
			Metadata struct {
				Name, Namespace string
			} `yaml:"metadata"`
			StringData map[string]string `yaml:"stringData"`
			Data       map[string]string `yaml:"data"`
		}
		if err := raw.Decode(&doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if doc.Kind != "Secret" {
			continue
		}
		if doc.StringData != nil {
			t.Errorf("%s: %s/%s uses stringData; server-side apply needs data", path, doc.Metadata.Namespace, doc.Metadata.Name)
		}
		values := map[string]string{}
		for k, v := range doc.Data {
			decoded, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatalf("%s: %s is not base64: %v", path, k, err)
			}
			values[k] = string(decoded)
		}
		id := doc.Metadata.Namespace + "/" + doc.Metadata.Name
		if _, dup := out[id]; dup {
			t.Errorf("%s: a second document for Secret %s; server-side apply keeps only the keys of the last", path, id)
		}
		out[id] = values
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
	spec, stack := filledInputs(t, true, nil)
	out := filepath.Join(t.TempDir(), "rendered")
	result, err := Render(context.Background(), siteRender(spec, stack, out))
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
	td := spec.Spire.TrustDomain
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	if dig(ctl, "spiffe", "trustDomain") != td || dig(ctl, "spiffe", "spire", "serverAddress") != vip ||
		dig(ctl, "nvkeOIDCAuth", "discoveryURL") != "https://"+vip+":30443/.well-known/openid-configuration" {
		t.Errorf("controller values: %v", ctl)
	}
	if _, has := ctl["image"]; has {
		t.Error("controller image set without an override")
	}
	// No values file carries a credential: every chart names the Secret.
	if dig(ctl, "secrets", "dbSecretName") != "nvke-database" || ctl["appDb"] != nil {
		t.Errorf("controller database values: %v %v", dig(ctl, "secrets"), ctl["appDb"])
	}
	if dig(ctl, "profiles", "image", "tag") != "v1" || dig(ctl, "profiles", "image", "digest") != "" {
		t.Errorf("profiles image: %v", ctl["profiles"])
	}
	pg := readValues(t, filepath.Join(out, "postgresql", SiteValuesFile))
	tp := readValues(t, filepath.Join(out, "temporal", SiteValuesFile))
	if dig(pg, "auth", "existingSecret") != "nvke-database" || dig(pg, "auth", "password") != nil ||
		dig(tp, "server", "config", "persistence", "default", "sql", "existingSecret") != "nvke-database" ||
		dig(tp, "server", "config", "persistence", "visibility", "sql", "existingSecret") != "nvke-database" {
		t.Errorf("database Secret references: %v %v", pg["auth"], dig(tp, "server", "config", "persistence"))
	}
	// The Secrets themselves are an item of their own, right after the
	// namespaces, in the namespaces the items' release.yaml name, with the
	// password from the environment and the controller's connection string.
	secrets := readSecrets(t, filepath.Join(out, "site-credentials", "secrets.yaml"))
	if secrets["postgres/nvke-database"]["postgres-password"] != "db-secret" || secrets["postgres/nvke-database"]["password"] != "db-secret" ||
		secrets["temporal/nvke-database"]["password"] != "db-secret" ||
		secrets["nvke-controller/nvke-database"]["database-connection-string"] != "postgresql://postgres:db-secret@postgres-postgresql.postgres.svc.cluster.local:5432/nvke-controller?sslmode=disable" {
		t.Errorf("site credentials: %v", secrets)
	}
	if secrets["spire-server/spire-upstream-ca"]["tls.crt"] != "CERT\n" || secrets["spire-server/spire-upstream-ca"]["tls.key"] != "KEY\n" || secrets["spire-server/spire-upstream-ca"]["bundle.crt"] != "CERT\n" {
		t.Errorf("spire CA Secret: %v", secrets["spire-server/spire-upstream-ca"])
	}
	if items, err := Items(out); err != nil || len(items) < 2 || items[0].Name != "nvke-base-config" || items[1].Name != "site-credentials" || items[1].Type != TypeManifest {
		t.Errorf("item list: %v %v", items, err)
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
	if ids, _ := dig(sp, "spire-server", "adminIDs").([]any); len(ids) != 1 || ids[0] != "spiffe://"+td+"/nvke-service/nvke-controller" {
		t.Errorf("adminIDs: %v", dig(sp, "spire-server", "adminIDs"))
	}
	if dig(sp, "spire-server", "nodeAttestor", "tpmDirect", "checksum") != "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16" {
		t.Errorf("arm64 TPM checksum: %v", dig(sp, "spire-server", "nodeAttestor", "tpmDirect", "checksum"))
	}
	// The CA is a Secret the site-credentials item holds; the chart creates
	// none and the values name it.
	if dig(sp, "spire-server", "upstreamAuthority", "disk", "enabled") != true || dig(sp, "spire-server", "upstreamAuthority", "disk", "secret", "create") != false ||
		dig(sp, "spire-server", "upstreamAuthority", "disk", "secret", "name") != "spire-upstream-ca" ||
		dig(sp, "spire-server", "upstreamAuthority", "disk", "secret", "data", "bundle") == "" {
		t.Errorf("upstream authority: %v", dig(sp, "spire-server", "upstreamAuthority"))
	}
	if spireRel, _ := ReadRelease(filepath.Join(out, "spire")); len(spireRel.Chart.SetFile) != 0 {
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
	// The credential list names the rendered Secret manifests and nothing
	// else: no values file holds a secret. The tree ignores itself for git.
	want := []string{"site-credentials/secrets.yaml"}
	for _, f := range []string{"20-nico-credentials.yaml", "35-dgxc-tenant-credentials.yaml"} {
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
	// No CA, a mirror and an image override.
	spec, stack := filledInputs(t, false, nil)
	stack.Mirror = "mirror.example.test:5000"
	stack.Images.Controller = "mirror.example.test:5000/cwe@sha256:" + strings.Repeat("a", 64)
	out := filepath.Join(t.TempDir(), "rendered")
	if _, err := Render(context.Background(), siteRender(spec, stack, out)); err != nil {
		t.Fatal(err)
	}
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	if dig(ctl, "image", "digest") != "sha256:"+strings.Repeat("a", 64) {
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
	if dig(tp, "server", "config", "persistence", "default", "sql", "existingSecret") != "nvke-database" ||
		dig(tp, "server", "config", "persistence", "visibility", "sql", "existingSecret") != "nvke-database" {
		t.Errorf("the mirror dropped temporal's Secret references: %v", tp["server"])
	}
	secrets := readSecrets(t, filepath.Join(out, "site-credentials", "secrets.yaml"))
	if len(secrets) != 3 || secrets["spire-server/spire-upstream-ca"] != nil {
		t.Errorf("site credentials without a CA: %v", secrets)
	}
	if _, err := os.Stat(filepath.Join(out, "nktp", SiteValuesFile)); err == nil {
		t.Error("nktp got site values; its overrides come from --values-for")
	}
}

func TestRender_DatabasePassword(t *testing.T) {
	// Without the variable the render refuses and leaves nothing.
	spec, stack := filledInputs(t, false, nil)
	t.Setenv(stackconfig.EnvDatabasePassword, "")
	out := filepath.Join(t.TempDir(), "rendered")
	_, err := Render(context.Background(), siteRender(spec, stack, out))
	if err == nil || !strings.Contains(err.Error(), stackconfig.EnvDatabasePassword+" is not set") {
		t.Errorf("unset password: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused render left its output behind")
	}
}

// With external credentials the tree holds no secret: no credential
// variable is read, the site-credentials item is not written, the template
// items' Secrets are dropped, and the render lists what the cluster has to
// hold.
func TestRender_ExternalCredentials(t *testing.T) {
	spec, stack := filledInputs(t, true, nil)
	for _, name := range []string{stackconfig.EnvNicoToken, stackconfig.EnvNodeUser, stackconfig.EnvNodePassword, stackconfig.EnvDatabasePassword} {
		t.Setenv(name, "")
	}
	// The CA files are not read: sitespec.LoadDescription leaves the paths
	// empty, and upstreamCA alone says the site uses one.
	for _, f := range []string{spec.Spire.CACertFile, spec.Spire.CAKeyFile} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	spec.Spire.CACertFile, spec.Spire.CAKeyFile = "", ""
	t.Setenv(sitespec.EnvSpireCACertFile, "")
	t.Setenv(sitespec.EnvSpireCAKeyFile, "")
	out := filepath.Join(t.TempDir(), "rendered")
	opts := siteRender(spec, stack, out)
	opts.Credentials = CredentialsExternal
	result, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, ".gitignore")); err == nil {
		t.Error("an external render wrote .gitignore")
	}
	if len(result.Credentials) != 0 {
		t.Errorf("credentials in an external render: %v", result.Credentials)
	}
	for _, f := range []string{"site-credentials", "nvke-base-config/20-nico-credentials.yaml", "nvke-base-config/35-dgxc-tenant-credentials.yaml"} {
		if _, err := os.Stat(filepath.Join(out, f)); err == nil {
			t.Errorf("%s written with external credentials", f)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "nvke-base-config", "45-nktp-cluster.yaml")); err != nil {
		t.Error("the non-secret template was not rendered")
	}
	items, _ := Items(out)
	for _, item := range items {
		if item.Name == "site-credentials" {
			t.Error("site-credentials in the item list")
		}
	}
	pg := readValues(t, filepath.Join(out, "postgresql", SiteValuesFile))
	if dig(pg, "auth", "existingSecret") != "nvke-database" {
		t.Errorf("the charts still name the Secret: %v", pg)
	}
	want := []string{
		"Secret postgres/nvke-database with keys password, postgres-password",
		"Secret temporal/nvke-database with keys password",
		"Secret nvke-controller/nvke-database with keys database-connection-string, the URL postgresql://postgres:<password>@postgres-postgresql.postgres.svc.cluster.local:5432/nvke-controller?sslmode=disable with the password URL-escaped",
		"Secret spire-server/spire-upstream-ca with keys bundle.crt, tls.crt, tls.key",
		"Secret capnico-system/nico-credentials with keys apiName, endpoint, orgID, token",
	}
	for _, w := range want {
		if !contains(result.Expected, w) {
			t.Errorf("expected list lacks %q:\n%s", w, strings.Join(result.Expected, "\n"))
		}
	}
	opts.Out, opts.Credentials = filepath.Join(t.TempDir(), "r"), "vault"
	if _, err := Render(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "credentials must be rendered or external") {
		t.Errorf("unknown credentials mode: %v", err)
	}
}

// A template file may hold a Secret beside another resource, and an item
// may install into a namespace of its own: each document is classified by
// its kind, and each Secret goes where its item's release.yaml says.
func TestRender_TemplateDocumentsAndNamespaces(t *testing.T) {
	spec, stack := filledInputs(t, false, nil)
	path := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, path); err != nil {
		t.Fatal(err)
	}
	mixed := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: site-name\n  namespace: capnico-system\ndata:\n  name: \"{{ .ClusterName }}\"\n--- # the pull secret\n" +
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: registry-pull\n  namespace: capnico-system\ntype: kubernetes.io/dockerconfigjson\nstringData:\n  .dockerconfigjson: \"{{ .Nico.Token }}\"\n"
	if err := os.WriteFile(filepath.Join(path, "nvke-base-config", "50-mixed.yaml.tmpl"), []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	pgRelease := filepath.Join(path, "postgresql", "release.yaml")
	data, err := os.ReadFile(pgRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pgRelease, bytes.Replace(data, []byte("namespace: postgres\n"), []byte("namespace: db\n"), 1), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "rendered")
	opts := siteRender(spec, stack, out)
	opts.DeployPath = path
	result, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _ := os.ReadFile(filepath.Join(out, "nvke-base-config", "50-mixed.yaml"))
	if !strings.Contains(string(rendered), "kind: ConfigMap") || !strings.Contains(string(rendered), `.dockerconfigjson: "nico-secret"`) {
		t.Errorf("rendered mixed template:\n%s", rendered)
	}
	if !contains(result.Credentials, "nvke-base-config/50-mixed.yaml") {
		t.Errorf("the mixed file is not listed as a credential: %v", result.Credentials)
	}
	secrets := readSecrets(t, filepath.Join(out, "site-credentials", "secrets.yaml"))
	if secrets["db/nvke-database"]["password"] != "db-secret" || secrets["postgres/nvke-database"] != nil {
		t.Errorf("the database Secret did not follow the postgresql item's namespace: %v", secrets)
	}

	for _, name := range []string{stackconfig.EnvNicoToken, stackconfig.EnvNodeUser, stackconfig.EnvNodePassword, stackconfig.EnvDatabasePassword} {
		t.Setenv(name, "")
	}
	opts.Out, opts.Credentials = filepath.Join(t.TempDir(), "external"), CredentialsExternal
	result, err = Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err = os.ReadFile(filepath.Join(opts.Out, "nvke-base-config", "50-mixed.yaml"))
	if err != nil || !strings.Contains(string(rendered), "kind: ConfigMap") || strings.Contains(string(rendered), "kind: Secret") {
		t.Errorf("external mixed template: %v\n%s", err, rendered)
	}
	for _, w := range []string{
		"Secret capnico-system/registry-pull with keys .dockerconfigjson, type kubernetes.io/dockerconfigjson",
		"Secret db/nvke-database with keys password, postgres-password",
	} {
		if !contains(result.Expected, w) {
			t.Errorf("expected list lacks %q:\n%s", w, strings.Join(result.Expected, "\n"))
		}
	}
}

// Items that read the one Secret name from one namespace share a single
// Secret holding every key they read: a second document for it would, under
// server-side apply, take away the keys the first one set.
func TestRender_SecretsSharingANamespace(t *testing.T) {
	spec, stack := filledInputs(t, true, nil)
	path := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, path); err != nil {
		t.Fatal(err)
	}
	for item, namespace := range map[string]string{"temporal": "temporal", "controller": "nvke-controller"} {
		release := filepath.Join(path, item, "release.yaml")
		data, err := os.ReadFile(release)
		if err != nil {
			t.Fatal(err)
		}
		moved := bytes.Replace(data, []byte("  namespace: "+namespace+"\n"), []byte("  namespace: postgres\n"), 1)
		if bytes.Equal(moved, data) {
			t.Fatalf("%s: release.yaml names no namespace %s", item, namespace)
		}
		if err := os.WriteFile(release, moved, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "rendered")
	opts := siteRender(spec, stack, out)
	opts.DeployPath = path
	if _, err := Render(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "site-credentials", "secrets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	docs, err := yamlDocuments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Errorf("secrets.yaml holds %d documents, want the shared database Secret and the CA Secret:\n%s", len(docs), raw)
	}
	shared := readSecrets(t, filepath.Join(out, "site-credentials", "secrets.yaml"))["postgres/nvke-database"]
	if shared["postgres-password"] != "db-secret" || shared["password"] != "db-secret" ||
		shared[connectionStringKey] != "postgresql://postgres:db-secret@postgres-postgresql.postgres.svc.cluster.local:5432/nvke-controller?sslmode=disable" || len(shared) != 3 {
		t.Errorf("the shared Secret does not hold every key: %v", shared)
	}

	opts.Out, opts.Credentials = filepath.Join(t.TempDir(), "external"), CredentialsExternal
	result, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "Secret postgres/nvke-database with keys password, postgres-password, database-connection-string, the URL " +
		"postgresql://postgres:<password>@postgres-postgresql.postgres.svc.cluster.local:5432/nvke-controller?sslmode=disable with the password URL-escaped"
	var lines []string
	for _, e := range result.Expected {
		if strings.HasPrefix(e, "Secret postgres/nvke-database ") {
			lines = append(lines, e)
		}
	}
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("expected lines for the shared Secret:\n%s\nwant one:\n%s", strings.Join(lines, "\n"), want)
	}
}

// mergeSecrets joins by namespace and name in first-seen order and refuses
// two values for one key.
func TestMergeSecrets(t *testing.T) {
	got, err := mergeSecrets([]siteSecret{
		{"a", "s", map[string]string{"x": "1"}},
		{"b", "s", map[string]string{"x": "1"}},
		{"a", "s", map[string]string{"y": "2", "x": "1"}},
	})
	if err != nil || len(got) != 2 || got[0].namespace != "a" || got[1].namespace != "b" ||
		len(got[0].data) != 2 || got[0].data["y"] != "2" || len(got[1].data) != 1 {
		t.Errorf("merged: %+v %v", got, err)
	}
	if _, err := mergeSecrets([]siteSecret{{"a", "s", map[string]string{"x": "1"}}, {"a", "s", map[string]string{"x": "2"}}}); err == nil ||
		!strings.Contains(err.Error(), "a/s") || !strings.Contains(err.Error(), "x") {
		t.Errorf("one key, two values: %v", err)
	}
}

// An external tree is meant for a repository: no file in it names a path of
// the machine that rendered it, and a committed chart path stays as committed.
func TestRender_ExternalTreeHoldsNoLocalPath(t *testing.T) {
	spec, stack := filledInputs(t, true, nil)
	out := filepath.Join(t.TempDir(), "rendered")
	opts := siteRender(spec, stack, out)
	opts.Credentials = CredentialsExternal
	result, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(nico)
	if err != nil {
		t.Fatal(err)
	}
	checkNoLocalPath(t, out, repositoryRoots(t, abs)...)
	release, err := ReadRelease(filepath.Join(out, "controller"))
	if err != nil || release.Chart == nil || release.Chart.Path != "../../controller/deploy/charts/controller" {
		t.Errorf("controller chart path: %+v %v", release, err)
	}
	if !contains(result.Notes, "controller: chart.path ../../controller/deploy/charts/controller stays as committed and resolves from where the tree is installed; render with --release to put the chart in the tree") {
		t.Errorf("notes: %v", result.Notes)
	}
	// The rendered default keeps writing the path absolute.
	rendered := filepath.Join(t.TempDir(), "rendered")
	if _, err := Render(context.Background(), siteRender(spec, stack, rendered)); err != nil {
		t.Fatal(err)
	}
	release, err = ReadRelease(filepath.Join(rendered, "controller"))
	if err != nil || release.Chart == nil || !filepath.IsAbs(release.Chart.Path) {
		t.Errorf("rendered controller chart path: %+v %v", release, err)
	}
}

// repositoryRoots names the spellings of the directory above deploy/ for a
// deploy path: as given and with symlinks resolved.
func repositoryRoots(t *testing.T, deployPath string) []string {
	t.Helper()
	roots := []string{filepath.Dir(filepath.Dir(deployPath))}
	if resolved, err := filepath.EvalSymlinks(deployPath); err == nil {
		roots = append(roots, filepath.Dir(filepath.Dir(resolved)))
	}
	return roots
}

// checkNoLocalPath fails for every file under dir that contains one of paths.
func checkNoLocalPath(t *testing.T, dir string, paths ...string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, p := range paths {
			if bytes.Contains(data, []byte(p)) {
				t.Errorf("%s names the local path %s", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRender_RefusesSharedTemp(t *testing.T) {
	spec, stack := filledInputs(t, false, nil)
	shared := filepath.Join("/tmp", fmt.Sprintf("nvke-render-refused-%d", os.Getpid()))
	for _, out := range []string{shared, filepath.Join(t.TempDir(), "rendered")} {
		_, err := Render(context.Background(), RenderOptions{DeployPath: nico, Site: spec, Stack: stack, Arch: "linux/arm64", Out: out})
		if err == nil || !strings.Contains(err.Error(), "is under a shared temp directory") {
			t.Errorf("%s: %v", out, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s was created", out)
		}
	}
}

func TestRender_SiteSpecRefusals(t *testing.T) {
	// The two inputs go together, the stack config comes from Load, and the
	// architecture is one of two; nothing is left behind.
	spec, stack := filledInputs(t, false, nil)
	out := filepath.Join(t.TempDir(), "rendered")
	for name, tc := range map[string]struct {
		opts RenderOptions
		want string
	}{
		"site without stack":  {RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Arch: "linux/arm64", Out: out}, "--sitespec needs --stack-config: the stack's input is the stack config file"},
		"stack without site":  {RenderOptions{AllowTempOut: true, DeployPath: nico, Stack: stack, Arch: "linux/arm64", Out: out}, "--stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec"},
		"stack not loaded":    {RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Stack: &stackconfig.Config{Nico: stack.Nico}, Arch: "linux/arm64", Out: out}, "must be loaded by stackconfig.Load"},
		"no architecture":     {RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Stack: stack, Out: out}, `the architecture "" is not one of linux/amd64, linux/arm64`},
		"architecture suffix": {RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Stack: stack, Arch: "arm64", Out: out}, `the architecture "arm64" is not one of`},
	} {
		_, err := Render(context.Background(), tc.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s: a refused render left its output behind", name)
		}
	}
	// A secret that is not set.
	spec, stack = filledInputs(t, false, nil)
	t.Setenv(stackconfig.EnvNicoToken, "")
	_, err := Render(context.Background(), siteRender(spec, stack, out))
	if err == nil || !strings.Contains(err.Error(), "environment: "+stackconfig.EnvNicoToken+" is not set") {
		t.Errorf("unset token: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused render left its output behind")
	}
	// --out must not exist, and an existing one is left alone.
	if err := os.MkdirAll(filepath.Join(out, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec, stack = filledInputs(t, false, nil)
	if _, err := Render(context.Background(), siteRender(spec, stack, out)); err == nil || !strings.Contains(err.Error(), "exists") {
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
	copiedOpts := siteRender(spec, stack, inside)
	copiedOpts.DeployPath = copied
	if _, err := Render(context.Background(), copiedOpts); err == nil || !strings.Contains(err.Error(), "inside the deploy path") {
		t.Errorf("nested out accepted: %v", err)
	}
	if _, statErr := os.Stat(inside); statErr == nil {
		t.Error("nested out was created")
	}
	// An empty CA file is refused by variable: the Secret would carry
	// nothing.
	caSpec, caStack := filledInputs(t, true, nil)
	if err := os.WriteFile(caSpec.Spire.CAKeyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(context.Background(), siteRender(caSpec, caStack, filepath.Join(t.TempDir(), "r"))); err == nil || !strings.Contains(err.Error(), sitespec.EnvSpireCAKeyFile+": ") || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty CA key: %v", err)
	}
	// A secret the templates cannot carry is refused by variable.
	for name, content := range map[string]string{"quote": "tok\"en", "newline": "abc\ndef", "backslash": "a\\b"} {
		spec, stack := filledInputs(t, false, nil)
		t.Setenv(stackconfig.EnvNodePassword, content)
		_, err := Render(context.Background(), siteRender(spec, stack, filepath.Join(t.TempDir(), "r")))
		if err == nil || !strings.Contains(err.Error(), stackconfig.EnvNodePassword+": the value holds a character") {
			t.Errorf("%s in a password: %v", name, err)
		}
	}
	// A whitespace-only secret is trimmed to nothing and refused as unset.
	blank, blankStack := filledInputs(t, false, nil)
	t.Setenv(stackconfig.EnvNicoToken, " \n")
	if _, err := Render(context.Background(), siteRender(blank, blankStack, filepath.Join(t.TempDir(), "r"))); err == nil || !strings.Contains(err.Error(), "environment: "+stackconfig.EnvNicoToken+" is not set") {
		t.Errorf("whitespace-only token: %v", err)
	}
	// Refusals by field: no profiles image while the deploy path enables it, a
	// non-secret value the templates cannot carry, an image override with no pin.
	for name, tc := range map[string]struct {
		mutate func(*stackconfig.Config)
		want   string
	}{
		"profiles image": {func(s *stackconfig.Config) { s.Images.Profiles = "" }, "stack config: images.profiles is required"},
		"quoted orgID":   {func(s *stackconfig.Config) { s.Nico.OrgID = "org\"x" }, "stack config: nico.orgID: the value holds a character"},
		"unpinned image": {func(s *stackconfig.Config) { s.Images.Controller = "mirror.example.test/cwe" }, "stack config: images.controller: \"mirror.example.test/cwe\" names neither a tag nor a digest"},
	} {
		spec, stack := filledInputs(t, false, nil)
		tc.mutate(stack)
		out := filepath.Join(t.TempDir(), "r")
		_, err := Render(context.Background(), siteRender(spec, stack, out))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s: output left behind", name)
		}
	}
	// A malformed helm release.yaml is reported once, and a deploy path missing
	// one of the items the site values go to is refused.
	malformed := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, malformed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformed, "temporal", "release.yaml"), []byte("chart:\n  name: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, stack = filledInputs(t, false, nil)
	malformedOpts := siteRender(spec, stack, filepath.Join(t.TempDir(), "r"))
	malformedOpts.DeployPath = malformed
	_, err = Render(context.Background(), malformedOpts)
	if err == nil || strings.Count(err.Error(), "temporal/release.yaml") != 1 {
		t.Errorf("malformed release.yaml should be reported once: %v", err)
	}
	withoutTemporal := filepath.Join(t.TempDir(), "nico")
	if err := copyTree(nico, withoutTemporal); err != nil {
		t.Fatal(err)
	}
	itemList, _ := os.ReadFile(filepath.Join(withoutTemporal, "nvke-deploy.yaml"))
	if err := os.WriteFile(filepath.Join(withoutTemporal, "nvke-deploy.yaml"), []byte(strings.Replace(string(itemList), "  - {name: temporal, type: helm}\n", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	withoutTemporalOpts := siteRender(spec, stack, filepath.Join(t.TempDir(), "r"))
	withoutTemporalOpts.DeployPath = withoutTemporal
	_, err = Render(context.Background(), withoutTemporalOpts)
	if err == nil || !strings.Contains(err.Error(), "no temporal helm item") {
		t.Errorf("missing consumer item: %v", err)
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
	brokenOpts := siteRender(spec, stack, filepath.Join(t.TempDir(), "r"))
	brokenOpts.DeployPath = broken
	if _, err := Render(context.Background(), brokenOpts); err == nil || !strings.Contains(err.Error(), "no .yaml.tmpl file") {
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
	spacedOpts := siteRender(spec, stack, filepath.Join(t.TempDir(), "r"))
	spacedOpts.DeployPath = spaced
	_, err = Render(context.Background(), spacedOpts)
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

// registry is an in-memory store that records what was fetched.
type registry struct {
	*memory.Store
	fetched []string
}

func newRegistry() *registry { return &registry{Store: memory.New()} }

func (r *registry) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	r.fetched = append(r.fetched, desc.Digest.String())
	return r.Store.Fetch(ctx, desc)
}

func (r *registry) open(string) (oras.ReadOnlyTarget, error) { return r, nil }

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// chartManifest returns a chart manifest holding one archive layer.
func chartManifest(blob []byte) []byte {
	layer := ocispec.Descriptor{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Digest: godigest.FromBytes(blob), Size: int64(len(blob))}
	manifest, _ := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Layers: []ocispec.Descriptor{layer}})
	return manifest
}

// chartArtifact pushes a chart and its manifest to r under repo/name and
// returns the artifact and the archive's digest.
func chartArtifact(r *registry, repo, name string) (nvkrrelease.Artifact, string) {
	ctx := context.Background()
	blob := []byte("chart " + name)
	manifest := chartManifest(blob)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.FromBytes(manifest), Size: int64(len(manifest))}
	ref := repo + "/" + name + "@" + desc.Digest.String()
	if err := r.Push(ctx, ocispec.Descriptor{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Digest: godigest.FromBytes(blob), Size: int64(len(blob))}, bytes.NewReader(blob)); err != nil {
		panic(err)
	}
	if err := r.Push(ctx, desc, bytes.NewReader(manifest)); err != nil {
		panic(err)
	}
	if err := r.Tag(ctx, desc, ref); err != nil {
		panic(err)
	}
	return nvkrrelease.Artifact{Name: name, Kind: "helm-oci", Digest: desc.Digest.String(), Reference: ref}, digest(blob)
}

func imageArtifact(repo, name, arch string) nvkrrelease.Artifact {
	d := digest([]byte("image " + name + arch))
	return nvkrrelease.Artifact{Name: name, Kind: "oci", Digest: d, Reference: repo + "/" + name + "@" + d, Arch: arch}
}

// The release decides the version of a chart it carries: the rendered copy
// records it, and a difference from the pin is a warning, not a refusal.
func TestRender_ReleaseChartVersionWins(t *testing.T) {
	repo := "ghcr.io/example/recipes"
	cwe := imageArtifact(repo, "cwe", "")
	profiles := imageArtifact(repo, "controller-profiles", "")
	capARM := imageArtifact(repo, "capnico-controller", "linux/arm64")
	for name, tc := range map[string]struct {
		version, recorded, warning string
	}{
		"another version": {"9.9.9", "9.9.9", "warning: temporal: the release carries temporal 9.9.9, release.yaml pins 0.74.0"},
		"no version":      {"", "0.74.0", ""},
	} {
		p := newRegistry()
		temporal, _ := chartArtifact(p, repo, "temporal")
		temporal.ChartVersion = tc.version
		release := &nvkrrelease.Release{Reference: repo + "/releases@" + digest([]byte("r")), Outputs: []nvkrrelease.Artifact{temporal, cwe, profiles, capARM}}
		out := filepath.Join(t.TempDir(), "r")
		var log strings.Builder
		if _, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/arm64", Out: out, Registry: p.open, Log: &log}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rel, _ := ReadRelease(filepath.Join(out, "temporal"))
		if rel.Chart.Version != tc.recorded {
			t.Errorf("%s: recorded version %q, want %q", name, rel.Chart.Version, tc.recorded)
		}
		if got := strings.Contains(log.String(), "warning: temporal"); got != (tc.warning != "") || (tc.warning != "" && !strings.Contains(log.String(), tc.warning)) {
			t.Errorf("%s: log:\n%s", name, log.String())
		}
	}
}

func TestRender_TrustDomainDefaultsWithAWarning(t *testing.T) {
	spec, stack := filledInputs(t, false, nil)
	spec.Spire.TrustDomain = ""
	out := filepath.Join(t.TempDir(), "r")
	var log strings.Builder
	opts := siteRender(spec, stack, out)
	opts.Log = &log
	if _, err := Render(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(filepath.Join(out, "site.env"))
	if !strings.Contains(string(env), "SITE_TRUST_DOMAIN=nvkectl-bootstrap.nke.local\n") {
		t.Errorf("site.env:\n%s", env)
	}
	if !strings.Contains(log.String(), "warning: spire.trustDomain is not set; using nvkectl-bootstrap.nke.local") {
		t.Errorf("no warning in the log:\n%s", log.String())
	}
}

func TestRender_FromRegistry(t *testing.T) {
	p := newRegistry()
	repo := "ghcr.io/example/recipes"
	temporal, temporalLayer := chartArtifact(p, repo, "temporal")
	controllerChart, controllerLayer := chartArtifact(p, repo, "controller-chart")
	// temporal is a repository chart and controller a path chart; the
	// release's version is recorded for both, and a differing pin is only
	// warned about.
	temporal.ChartVersion, controllerChart.ChartVersion = "0.74.0", "0.1.1"
	cwe := imageArtifact(repo, "cwe", "")
	profiles := imageArtifact(repo, "controller-profiles", "")
	capAMD := imageArtifact(repo, "capnico-controller", "linux/amd64")
	capARM := imageArtifact(repo, "capnico-controller-arm64", "linux/arm64")
	// A release may reference an image as repository:tag@digest.
	capARM.Reference = repo + "/capnico-controller-arm64:v0.0.45@" + capARM.Digest
	release := &nvkrrelease.Release{Reference: repo + "/releases@" + digest([]byte("r")), Outputs: []nvkrrelease.Artifact{temporal, controllerChart, cwe, profiles, capAMD, capARM}}
	out := filepath.Join(t.TempDir(), "rendered")
	result, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/arm64", Out: out, Registry: p.open})
	if err != nil {
		t.Fatal(err)
	}
	// The committed path is untouched; the copy carries every file.
	if _, err := os.Stat(filepath.Join(nico, "charts")); err == nil {
		t.Fatal("the committed deploy path gained a charts directory")
	}
	for _, f := range []string{"nvke-deploy.yaml", "capnico/capnico.yaml", "nvke-base-config/00-namespaces.yaml", "controller/values.yaml"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s not copied: %v", f, err)
		}
	}
	// temporal comes from the archive; its release.yaml points at it.
	rel, err := ReadRelease(filepath.Join(out, "temporal"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "charts/temporal/temporal-" + strings.TrimPrefix(temporal.Digest, "sha256:") + ".tgz"
	if rel.Chart.Path != wantPath || rel.Chart.Repository != "" || rel.Chart.Version != "0.74.0" {
		t.Errorf("temporal release.yaml: %+v", rel.Chart)
	}
	if data, err := os.ReadFile(filepath.Join(out, wantPath)); err != nil || digest(data) != temporalLayer {
		t.Errorf("temporal archive: %v", err)
	}
	// postgresql is not in the release: untouched.
	pg, _ := ReadRelease(filepath.Join(out, "postgresql"))
	if pg.Chart.Repository == "" || pg.Chart.Path != "" {
		t.Errorf("postgresql should stay on its repository: %+v", pg.Chart)
	}
	// controller: chart from the archive, images in release-images.yaml.
	ctl, _ := ReadRelease(filepath.Join(out, "controller"))
	if !strings.HasPrefix(ctl.Chart.Path, "charts/controller/nvke-controller-") || ctl.Values[len(ctl.Values)-1] != "release-images.yaml" {
		t.Errorf("controller release.yaml: %+v", ctl)
	}
	values, _ := os.ReadFile(filepath.Join(out, "controller", "release-images.yaml"))
	want := "image:\n  digest: \"" + cwe.Digest + "\"\n  repository: \"" + repo + "/cwe\"\nprofiles:\n  image:\n    digest: \"" + profiles.Digest + "\"\n    repository: \"" + repo + "/controller-profiles\"\n"
	if !strings.HasSuffix(string(values), want) {
		t.Errorf("release-images.yaml:\n%s\nwant suffix:\n%s", values, want)
	}
	// capnico: kustomization with the arm64 artifact.
	kust, _ := os.ReadFile(filepath.Join(out, "capnico", "kustomization.yaml"))
	if !strings.Contains(string(kust), "  - capnico.yaml\n") || strings.Contains(string(kust), "release.yaml") ||
		!strings.Contains(string(kust), "newName: \""+repo+"/capnico-controller-arm64\"") || !strings.Contains(string(kust), "digest: \""+capARM.Digest+"\"") {
		t.Errorf("kustomization:\n%s", kust)
	}
	// images.txt lists the three images as repository@digest, the tag
	// dropped, without layouts.
	images, _ := os.ReadFile(filepath.Join(out, "images.txt"))
	// In deploy-path order: capnico comes before the controller.
	if string(images) != repo+"/capnico-controller-arm64@"+capARM.Digest+"\t\n"+cwe.Reference+"\t\n"+profiles.Reference+"\t\n" {
		t.Errorf("images.txt:\n%s", images)
	}
	if len(result.Images) != 3 {
		t.Errorf("images: %+v", result.Images)
	}
	// Only the two charts were fetched, never an image.
	charts := []string{temporal.Digest, temporalLayer, controllerChart.Digest, controllerLayer}
	for _, d := range p.fetched {
		if !contains(charts, d) {
			t.Errorf("fetched %s, which is not a chart", d)
		}
	}
}

func TestRender_Refusals(t *testing.T) {
	p := newRegistry()
	repo := "ghcr.io/example/recipes"
	capARM := imageArtifact(repo, "capnico-controller-arm64", "linux/arm64")
	profiles := imageArtifact(repo, "controller-profiles", "")
	release := &nvkrrelease.Release{Reference: "s", Outputs: []nvkrrelease.Artifact{capARM, profiles}}
	out := filepath.Join(t.TempDir(), "rendered")
	_, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/amd64", Out: out, Registry: p.open})
	if err == nil {
		t.Fatal("rendered without the controller image")
	}
	for _, want := range []string{"controller: image cwe is not in the release", "capnico: image: capnico-controller is published only for other architectures"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a failed render left its output behind")
	}
	// The output must not exist.
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/arm64", Out: out, Registry: p.open}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("existing out accepted: %v", err)
	}
	// A reference that resolves to another manifest than its digest is refused.
	bad := newRegistry()
	temporal, _ := chartArtifact(bad, repo, "temporal")
	temporal.ChartVersion = "0.74.0"
	tampered := temporal
	tampered.Digest = "sha256:" + strings.Repeat("0", 64)
	tampered.Reference = repo + "/temporal@" + tampered.Digest
	desc, _ := bad.Resolve(context.Background(), temporal.Reference)
	if err := bad.Tag(context.Background(), desc, tampered.Reference); err != nil {
		t.Fatal(err)
	}
	cwe := imageArtifact(repo, "cwe", "")
	rel := &nvkrrelease.Release{Reference: "s", Outputs: []nvkrrelease.Artifact{tampered, cwe, profiles, capARM}}
	_, err = Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: rel, Arch: "linux/arm64", Out: filepath.Join(t.TempDir(), "r"), Registry: bad.open})
	if err == nil || !strings.Contains(err.Error(), "the manifest is sha256:") {
		t.Errorf("tampered manifest: %v", err)
	}
}

// writeLayout writes an OCI layout holding manifest (and blob) under dir/name
// and returns the manifest digest.
func writeLayout(t *testing.T, dir, name string, manifest, blob []byte) string {
	t.Helper()
	layout := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(layout, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := digest(manifest)
	if err := os.WriteFile(filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(d, "sha256:")), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if blob != nil {
		if err := os.WriteFile(filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(digest(blob), "sha256:")), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []map[string]any{{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": d, "size": len(manifest)}}})
	if err := os.WriteFile(filepath.Join(layout, "index.json"), index, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRender_FromRun(t *testing.T) {
	run := t.TempDir()
	packages := filepath.Join(run, "packages")
	blob := []byte("temporal chart bytes")
	tManifest := chartManifest(blob)
	tDigest := writeLayout(t, packages, "temporal", tManifest, blob)
	cweDigest := writeLayout(t, packages, "cwe", []byte(`{"config":{"digest":"sha256:0"}}`), nil)
	profDigest := writeLayout(t, packages, "controller-profiles", []byte(`{"config":{"digest":"sha256:1"}}`), nil)
	capDigest := writeLayout(t, packages, "capnico-controller-arm64", []byte(`{"config":{"digest":"sha256:2"}}`), nil)
	ref := func(name, d string) string { return "nvkr.local/nvke/" + name + "@" + d }
	release := &nvkrrelease.Release{Reference: "nvkr.local/nvke/releases@" + digest([]byte("x")), Outputs: []nvkrrelease.Artifact{
		{Name: "temporal", Kind: "helm-oci", Digest: tDigest, Reference: ref("temporal", tDigest), ChartVersion: "0.74.0"},
		{Name: "cwe", Kind: "oci", Digest: cweDigest, Reference: ref("cwe", cweDigest)},
		{Name: "controller-profiles", Kind: "oci", Digest: profDigest, Reference: ref("controller-profiles", profDigest)},
		{Name: "capnico-controller-arm64", Kind: "oci", Digest: capDigest, Reference: ref("capnico-controller-arm64", capDigest), Arch: "linux/arm64"},
	}}
	out := filepath.Join(t.TempDir(), "rendered")
	result, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/arm64", RunDir: run, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "charts", "temporal", "temporal-"+strings.TrimPrefix(tDigest, "sha256:")+".tgz"))
	if err != nil || string(data) != string(blob) {
		t.Errorf("chart copied from the layout: %v", err)
	}
	images, _ := os.ReadFile(filepath.Join(out, "images.txt"))
	want := ref("capnico-controller-arm64", capDigest) + "\t" + filepath.Join(packages, "capnico-controller-arm64") + "\n" +
		ref("cwe", cweDigest) + "\t" + filepath.Join(packages, "cwe") + "\n" +
		ref("controller-profiles", profDigest) + "\t" + filepath.Join(packages, "controller-profiles") + "\n"
	if string(images) != want {
		t.Errorf("images.txt:\n%s\nwant:\n%s", images, want)
	}
	if len(result.Images) != 3 || result.Images[0].Layout == "" {
		t.Errorf("images: %+v", result.Images)
	}
	// An external tree names the images and no layout: a layout is a
	// directory of this machine.
	externalOut := filepath.Join(t.TempDir(), "external")
	if _, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/arm64", RunDir: run, Out: externalOut, Credentials: CredentialsExternal}); err != nil {
		t.Fatal(err)
	}
	externalImages, _ := os.ReadFile(filepath.Join(externalOut, "images.txt"))
	wantExternal := ref("capnico-controller-arm64", capDigest) + "\t\n" + ref("cwe", cweDigest) + "\t\n" + ref("controller-profiles", profDigest) + "\t\n"
	if string(externalImages) != wantExternal {
		t.Errorf("external images.txt:\n%s\nwant:\n%s", externalImages, wantExternal)
	}
	abs, _ := filepath.Abs(nico)
	checkNoLocalPath(t, externalOut, append(repositoryRoots(t, abs), run)...)
	// A layout the Run lacks is refused, nothing left behind.
	if err := os.RemoveAll(filepath.Join(packages, "cwe")); err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(t.TempDir(), "rendered")
	_, err = Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Release: release, Arch: "linux/arm64", RunDir: run, Out: out2})
	if err == nil || !strings.Contains(err.Error(), "no OCI layout") {
		t.Errorf("missing layout: %v", err)
	}
	if _, statErr := os.Stat(out2); statErr == nil {
		t.Error("output left behind")
	}
}

func TestRender_SiteAndRelease(t *testing.T) {
	p := newRegistry()
	repo := "ghcr.io/example/recipes"
	temporal, _ := chartArtifact(p, repo, "temporal")
	temporal.ChartVersion = "0.74.0"
	cwe := imageArtifact(repo, "cwe", "")
	profiles := imageArtifact(repo, "controller-profiles", "")
	capARM := imageArtifact(repo, "capnico-controller-arm64", "linux/arm64")
	release := &nvkrrelease.Release{Reference: repo + "/releases@" + digest([]byte("r")), Outputs: []nvkrrelease.Artifact{temporal, cwe, profiles, capARM}}
	out := filepath.Join(t.TempDir(), "rendered")
	spec, stack := filledInputs(t, false, nil)
	stack.Images.Controller = "mirror.example.test/nvke/cwe:v1.2.3"
	if _, err := Render(context.Background(), RenderOptions{AllowTempOut: true, DeployPath: nico, Site: spec, Stack: stack, Release: release, Arch: "linux/arm64", Out: out, Registry: p.open}); err != nil {
		t.Fatal(err)
	}
	// A tag-only override replaces the release image completely: the site
	// values clear the digest release-images.yaml set.
	ctl := readValues(t, filepath.Join(out, "controller", SiteValuesFile))
	if img, _ := ctl["image"].(map[string]any); img["tag"] != "v1.2.3" || img["digest"] != "" {
		t.Errorf("tag override beside a release: %v", ctl["image"])
	}
	// The controller carries both generated values files, release first,
	// site last, after the committed ones; its chart is the committed one,
	// written absolute, since the release lacks controller-chart.
	rel, _ := ReadRelease(filepath.Join(out, "controller"))
	if len(rel.Values) != 4 || rel.Values[2] != "release-images.yaml" || rel.Values[3] != SiteValuesFile ||
		!filepath.IsAbs(rel.Chart.Path) || !strings.HasSuffix(rel.Chart.Path, filepath.Join("controller", "deploy", "charts", "controller")) {
		t.Errorf("controller release.yaml: %+v %+v", rel, rel.Chart)
	}
	tmp, _ := ReadRelease(filepath.Join(out, "temporal"))
	if !strings.HasPrefix(tmp.Chart.Path, "charts/temporal/") || tmp.Values[len(tmp.Values)-1] != SiteValuesFile {
		t.Errorf("temporal release.yaml: %+v", tmp)
	}
	if _, err := os.Stat(filepath.Join(out, "capnico", "kustomization.yaml")); err != nil {
		t.Error("capnico kustomization missing")
	}
	items, _ := Items(out)
	if items[0].Type != TypeManifest {
		t.Errorf("template item not rendered: %+v", items[0])
	}
}

// The TPM attestor checksum follows the architecture the render was given,
// since the nodes' architecture lives in no file.
func TestRender_ChecksumPerArchitecture(t *testing.T) {
	for arch, want := range map[string]string{"linux/amd64": "46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc", "linux/arm64": "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16"} {
		spec, stack := filledInputs(t, false, nil)
		out := filepath.Join(t.TempDir(), "r")
		opts := siteRender(spec, stack, out)
		opts.Arch = arch
		if _, err := Render(context.Background(), opts); err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		sp := readValues(t, filepath.Join(out, "spire", SiteValuesFile))
		if got := dig(sp, "spire-server", "nodeAttestor", "tpmDirect", "checksum"); got != want {
			t.Errorf("%s: checksum %v, want %s", arch, got, want)
		}
	}
}
