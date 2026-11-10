// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalogtemplate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
)

const template = "../../../aicr-catalog"

var digest = "sha256:" + strings.Repeat("a", 64)

// release builds a description with the given outputs, each a JSON object body.
func release(t *testing.T, outputs ...string) *nvkrrelease.Release {
	t.Helper()
	doc := `{"schemaVersion":"nvkr.dev/release-description/v1","reference":"r.example/nvke/releases@` + digest +
		`","rootDigest":"` + digest + `","outputs":[` + strings.Join(outputs, ",") + `]}`
	r, err := nvkrrelease.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func chart(name, version string) string {
	return `{"name":"` + name + `","kind":"helm-oci","digest":"` + digest + `","reference":"r.example/nvke/` + name + `@` + digest + `","chartVersion":"` + version + `"}`
}

func read(t *testing.T, dir, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRender_RefusesUnpublishedChartsWithoutARelease(t *testing.T) {
	for _, target := range []string{"nvke-seed", "nvke-site"} {
		_, err := Render(Options{TemplateDir: template, OutDir: filepath.Join(t.TempDir(), "out"), Target: target})
		if err == nil || !strings.Contains(err.Error(), "nke-controller") {
			t.Errorf("%s: want an unpublished-chart refusal naming nke-controller, got %v", target, err)
		}
	}
}

func TestRender_ReleaseSuppliesTheUnpublishedCharts(t *testing.T) {
	rel := release(t, chart("controller-chart", "0.23.0"), chart("nktp", "0.23.0"), chart("postgresql-dev", "0.1.1"))
	out := filepath.Join(t.TempDir(), "out")
	res, err := Render(Options{TemplateDir: template, OutDir: out, Target: "nvke-seed", Release: rel, Arch: "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Replaced) != 3 {
		t.Errorf("replaced = %v", res.Replaced)
	}
	reg := read(t, out, "registry.yaml")
	if !strings.Contains(reg, `defaultRepository: "oci://r.example/nvke"`) || !strings.Contains(reg, `defaultVersion: "0.23.0"`) {
		t.Errorf("registry.yaml not rewritten:\n%s", reg)
	}
	seed := read(t, out, "overlays/nvke-seed.yaml")
	if !strings.Contains(seed, `"oci://r.example/nvke"`) || !strings.Contains(seed, `chart: "postgresql-dev"`) {
		t.Errorf("seed overlay not rewritten:\n%s", seed)
	}
	if !strings.Contains(read(t, out, "registry.yaml"), "# cert-manager is not listed") {
		t.Error("registry.yaml lost its comments")
	}
}

func TestRender_SiteDoesNotNeedTheSeedsPostgres(t *testing.T) {
	rel := release(t, chart("controller-chart", "0.23.0"), chart("nktp", "0.23.0"))
	if _, err := Render(Options{TemplateDir: template, OutDir: filepath.Join(t.TempDir(), "out"), Target: "nvke-site", Release: rel, Arch: "linux/arm64"}); err != nil {
		t.Fatal(err)
	}
}

func TestRender_RefusesAnOutputOfTheWrongKind(t *testing.T) {
	rel := release(t, `{"name":"nktp","kind":"oci","digest":"`+digest+`","reference":"r.example/nvke/nktp@`+digest+`"}`)
	_, err := Render(Options{TemplateDir: template, OutDir: filepath.Join(t.TempDir(), "out"), Target: "nvke-site", Release: rel, Arch: "linux/arm64"})
	if err == nil || !strings.Contains(err.Error(), `"nktp" is a oci`) {
		t.Fatalf("got %v", err)
	}
}

func TestRender_PinsTheImageToTheReleaseDigest(t *testing.T) {
	rel := release(t, chart("controller-chart", "0.23.0"), chart("nktp", "0.23.0"),
		`{"name":"capnico-controller","kind":"oci","digest":"`+digest+`","reference":"r.example/nvke/capnico@`+digest+`"}`)
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Render(Options{TemplateDir: template, OutDir: out, Target: "nvke-site", Release: rel, Arch: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	manifest := read(t, out, "components/capnico/manifests/capnico.yaml")
	if !strings.Contains(manifest, "r.example/nvke/capnico@"+digest) || strings.Contains(manifest, "controller:v0.0.45") {
		t.Error("the CAPNICo image was not pinned to the release's digest")
	}
}

func TestRender_RefusesANonEmptyOutputDirectory(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(Options{TemplateDir: template, OutDir: out, Target: "nvke-site"}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadDescriptor_RefusesAnUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DescriptorFile), []byte("apiVersion: other/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDescriptor(dir); err == nil || !strings.Contains(err.Error(), "apiVersion") {
		t.Fatalf("got %v", err)
	}
}

// objects lists "kind/name" for every document in a manifest, ignoring Helm template directives and any
// Namespace: the template drops its Namespace objects (Helm creates them) and adds a selection ConfigMap.
func objects(t *testing.T, manifest string) []string {
	t.Helper()
	var out []string
	for _, doc := range strings.Split(manifest, "\n---") {
		var kind, name string
		inMeta := false
		for _, line := range strings.Split(doc, "\n") {
			switch {
			case strings.HasPrefix(line, "kind: "):
				kind = strings.TrimSpace(strings.TrimPrefix(line, "kind: "))
			case line == "metadata:":
				inMeta = true
			case inMeta && strings.HasPrefix(line, "  name: ") && name == "":
				name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "  name: ")), `"`)
			case !strings.HasPrefix(line, " ") && line != "metadata:" && !strings.HasPrefix(line, "{{") && line != "":
				inMeta = false
			}
		}
		if kind != "" && kind != "Namespace" && !strings.HasSuffix(name, "-selection") {
			out = append(out, kind+"/"+name)
		}
	}
	return out
}

// TestTemplateProviderManifestsMatchTheGeneratedOnes: the template's provisioned-provider and CAPNICo manifests
// are derived from the generated ones nvkectl still builds from (make provider-manifest, deploy/nico/capnico). A
// provider change that regenerates those without updating the template fails here, not in a bootstrap.
func TestTemplateProviderManifestsMatchTheGeneratedOnes(t *testing.T) {
	for _, tc := range []struct{ template, source string }{
		{"components/provisioned-provider/manifests/provisioned-provider.yaml", "../bootstrap/controllerdeploy/manifests/provisioned-provider.yaml"},
		{"components/capnico/manifests/capnico.yaml", "../bootstrap/controllerdeploy/manifests/capnico.yaml"},
	} {
		got := objects(t, read(t, template, tc.template))
		want := objects(t, read(t, ".", tc.source))
		if len(want) < 5 {
			t.Fatalf("%s: found only %d objects; the comparison is vacuous", tc.source, len(want))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: objects differ from %s\n got  %d objects\n want %d objects", tc.template, tc.source, len(got), len(want))
		}
	}
}
