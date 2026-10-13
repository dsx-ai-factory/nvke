// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkrrelease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(name string) string {
	return filepath.Join("..", "..", "..", "..", "deploy", "testdata", "nvkr-release", name)
}

func TestLoad_BadFixturesNameTheProblem(t *testing.T) {
	cases := map[string]string{
		"bad-schema.json":                `schemaVersion is "nvkr.dev/release/v2"`,
		"missing-digest.json":            "artifacts[0]: name, kind and digest are required",
		"bad-digest.json":                "artifact cwe: digest is not sha256:<64 hex>",
		"bad-reference.json":             "artifact cwe: reference is not an OCI reference",
		"reference-digest-mismatch.json": "does not end in its digest",
		"duplicate-name.json":            "duplicate artifact name: cwe",
		"bad-arch.json":                  "artifact capnico-controller: arch linux/x86 is not one of",
		"no-artifacts.json":              "artifacts is empty",
		"not-json.json":                  "not a JSON document",
	}
	for file, want := range cases {
		_, err := Load(fixture(file))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), file) {
			t.Errorf("%s: want %q naming the file, got %v", file, want, err)
		}
	}
	if _, err := Parse([]byte("  \n")); err == nil || err.Error() != "the file is empty" {
		t.Errorf("empty: %v", err)
	}
}

func TestLoad_ValidAndWrapped(t *testing.T) {
	for _, file := range []string{"valid.json", "wrapped.json", "legacy-v3.json"} {
		r, err := Load(fixture(file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if r.Lane != "pipeline-attested" || r.Scope != "artifact-publication" || len(r.Artifacts) != 15 {
			t.Errorf("%s: lane %q scope %q artifacts %d", file, r.Lane, r.Scope, len(r.Artifacts))
		}
		if !strings.HasSuffix(r.Subject, "/releases@sha256:"+strings.Repeat("0", 64)) {
			t.Errorf("%s: subject %s", file, r.Subject)
		}
	}
	r, _ := Load(fixture("valid.json"))
	if charts := r.OfKind("helm-oci"); len(charts) != 6 || charts[0].Name != "controller-chart" {
		t.Errorf("helm-oci artifacts: %+v", charts)
	}
	cwe, ok := r.Artifact("cwe")
	if !ok || cwe.Repository() != "ghcr.io/example/recipes/cwe" || cwe.Platform() != "" {
		t.Errorf("cwe: %+v %v", cwe, ok)
	}
	if a, _ := r.Artifact("capnico-controller-arm64"); a.Platform() != "linux/arm64" {
		t.Errorf("platform: %q", a.Platform())
	}
	tagged := Artifact{Reference: "registry.example.test:5000/nvke/cwe:v1@sha256:" + strings.Repeat("a", 64)}
	if tagged.Repository() != "registry.example.test:5000/nvke/cwe" {
		t.Errorf("repository drops the tag: %s", tagged.Repository())
	}
}

func TestSelect(t *testing.T) {
	r, err := Load(fixture("valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, arch, want, err string
	}{
		{"cwe", "linux/arm64", "cwe", ""},
		{"cwe", "linux/amd64", "cwe", ""},
		{"capnico-controller", "linux/arm64", "capnico-controller-arm64", ""},
		{"capnico-controller", "linux/amd64", "capnico-controller", ""},
		{"cluster-api-provider-provisioned", "linux/arm64", "cluster-api-provider-provisioned-arm64", ""},
		{"cluster-api-provider-provisioned", "linux/amd64", "", "published only for other architectures: cluster-api-provider-provisioned-arm64 (linux/arm64)"},
		{"spire-server-image", "linux/amd64", "", "published only for other architectures"},
		{"postgresql", "linux/arm64", "postgresql", ""},
		{"absent", "linux/arm64", "", "absent: not in the release"},
		{"cwe", "linux/mips", "", "arch linux/mips is not one of"},
	}
	for _, tc := range cases {
		got, err := r.Select(tc.name, tc.arch)
		if tc.err == "" {
			if err != nil || got.Name != tc.want {
				t.Errorf("Select(%s, %s) = %q, %v; want %q", tc.name, tc.arch, got.Name, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("Select(%s, %s): want error %q, got %v", tc.name, tc.arch, tc.err, err)
		}
	}
	if _, err := r.Select("absent", "linux/arm64"); !errors.Is(err, ErrNotPublished) {
		t.Errorf("absent is not ErrNotPublished: %v", err)
	}
	nr, err := Load(fixture("no-reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nr.Select("cwe", "linux/arm64"); err == nil || !strings.Contains(err.Error(), "artifact cwe has no reference") {
		t.Errorf("no reference: %v", err)
	}
}

func TestSelect_SuffixBeatsAnArchlessName(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	r := &Release{Artifacts: []Artifact{
		{Name: "x", Kind: "oci", Digest: d, Reference: "r.example/x@" + d},
		{Name: "x-arm64", Kind: "oci", Digest: d, Reference: "r.example/x-arm64@" + d},
	}}
	// Without arch fields the suffix is the only signal: the suffixed name is
	// the one built for arm64, and the plain name serves amd64.
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x-arm64" {
		t.Errorf("arm64: %q %v", got.Name, err)
	}
	if got, err := r.Select("x", "linux/amd64"); err != nil || got.Name != "x" {
		t.Errorf("amd64: %q %v", got.Name, err)
	}
	// With arch fields the name no longer matters.
	r.Artifacts[0].Arch = "linux/arm64"
	r.Artifacts[1].Arch = "linux/amd64"
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x" {
		t.Errorf("arch field wins: %q %v", got.Name, err)
	}
}

func TestChartLayer(t *testing.T) {
	manifest := `{"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:first"},` +
		`{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"sha256:chart","annotations":{"org.opencontainers.image.title":"spire-0.30.1.tgz"}}]}`
	layer, file, err := ChartLayer([]byte(manifest), "fallback.tgz")
	if err != nil || layer.Digest != "sha256:chart" || file != "spire-0.30.1.tgz" {
		t.Errorf("chart layer: %s %s %v", layer.Digest, file, err)
	}
	layer, file, err = ChartLayer([]byte(`{"layers":[{"digest":"sha256:only"}]}`), "fallback.tgz")
	if err != nil || layer.Digest != "sha256:only" || file != "fallback.tgz" {
		t.Errorf("first layer fallback: %s %s %v", layer.Digest, file, err)
	}
	if _, _, err := ChartLayer([]byte(`{"layers":[]}`), "x"); err == nil {
		t.Error("no layers accepted")
	}
	if _, _, err := ChartLayer([]byte(`nope`), "x"); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestChartLayer_TitleMustBeAFileName(t *testing.T) {
	for _, title := range []string{"../x.tgz", "a/b.tgz", ".", ".."} {
		manifest := `{"layers":[{"digest":"sha256:x","annotations":{"org.opencontainers.image.title":"` + title + `"}}]}`
		if _, file, _ := ChartLayer([]byte(manifest), "safe.tgz"); file != "safe.tgz" {
			t.Errorf("title %q was used as a file name: %s", title, file)
		}
	}
}

func TestFindLayout(t *testing.T) {
	dir := t.TempDir()
	index := func(name, digest string) {
		layout := filepath.Join(dir, name)
		if err := os.MkdirAll(layout, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(layout, "index.json"), []byte(`{"manifests":[{"digest":"`+digest+`"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aaaa, bbbb := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	index("cwe", aaaa)
	index("temporal", bbbb)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := FindLayout(dir, aaaa); err != nil || got != filepath.Join(dir, "cwe") {
		t.Errorf("by digest: %q %v", got, err)
	}
	if _, err := FindLayout(dir, "sha256:"+strings.Repeat("c", 64)); err == nil || !strings.Contains(err.Error(), "no OCI layout") {
		t.Errorf("absent digest: %v", err)
	}
	index("cwe-copy", aaaa)
	if _, err := FindLayout(dir, aaaa); err == nil || !strings.Contains(err.Error(), "several OCI layouts") {
		t.Errorf("duplicate digest: %v", err)
	}
	if got, err := FindLayout(filepath.Join(dir, "temporal"), bbbb); err != nil || got != filepath.Join(dir, "temporal") {
		t.Errorf("the dir itself: %q %v", got, err)
	}
	// A completed Run holds its layouts under packages/.
	run := t.TempDir()
	if err := os.MkdirAll(filepath.Join(run, "packages", "nktp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "packages", "nktp", "index.json"), []byte(`{"manifests":[{"digest":"`+bbbb+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := FindLayout(run, bbbb); err != nil || got != filepath.Join(run, "packages", "nktp") {
		t.Errorf("a run directory: %q %v", got, err)
	}
}
