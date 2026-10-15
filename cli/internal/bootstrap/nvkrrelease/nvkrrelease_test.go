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
		"bad-schema.json":                `schemaVersion is "nvkr.dev/release/v2", expected nvkr.dev/release-description/v1`,
		"release-v4.json":                "not the output of `nvkr release verify`",
		"verification-result.json":       `schemaVersion is "nvkr.dev/release-verification-result/v1"`,
		"filtered.json":                  `the description is filtered to "cwe"; export the whole release`,
		"empty-index.json":               "output cwe: imageIndex has no children",
		"missing-digest.json":            "outputs[0]: name, kind and digest are required",
		"bad-digest.json":                "output cwe: digest is not sha256:<64 hex>",
		"bad-reference.json":             "output cwe: reference is not an OCI reference",
		"reference-digest-mismatch.json": "does not end in its digest",
		"duplicate-name.json":            "duplicate output name: cwe",
		"bad-arch.json":                  `output capnico-controller: platform "linux/x86 64" is not os/arch`,
		"no-artifacts.json":              "outputs is empty",
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

func TestParse_ReferenceAndRootDigestAgree(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	out := `"outputs":[{"name":"cwe","kind":"oci","digest":"` + d + `"}]`
	cases := map[string]string{
		`{"schemaVersion":"nvkr.dev/release-description/v1","rootDigest":"` + d + `",` + out + `}`:                                                                  "reference is empty",
		`{"schemaVersion":"nvkr.dev/release-description/v1","reference":"r.example/x@` + d + `","rootDigest":"sha256:0",` + out + `}`:                               "rootDigest is not sha256:<64 hex>",
		`{"schemaVersion":"nvkr.dev/release-description/v1","reference":"r.example/x@` + d + `","rootDigest":"sha256:` + strings.Repeat("b", 64) + `",` + out + `}`: "does not end in rootDigest",
	}
	for doc, want := range cases {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestParse_RefusesMalformedOutputs(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	head := `{"schemaVersion":"nvkr.dev/release-description/v1","reference":"r.example/x@` + d + `","rootDigest":"` + d + `",`
	cases := map[string]string{
		`"outputs":[{"name":"a b","kind":"oci","digest":"` + d + `"}]`:                                                                                                                       `output name must be a token: "a b"`,
		`"outputs":[{"name":"x","kind":"helm oci","digest":"` + d + `"}]`:                                                                                                                    `output x: kind must be a token`,
		`"outputs":[{"name":"x","kind":"helm-oci","digest":"` + d + `","chartVersion":"1.0 beta"}]`:                                                                                          `output x: chartVersion is not a version`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","reference":"not-a-ref@` + d + `"}]`:                                                                                       `output x: reference is not an OCI reference`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","os":"linux"}]`:                                                                                                            `output x: os "linux" without arch`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","os":"windows","arch":"linux/amd64"}]`:                                                                                     `output x: os "windows" disagrees with arch`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","arch":"amd64","imageIndex":{"children":[{"platform":"linux/amd64","manifestDigest":"` + d + `"}]}}]`:                      `output x: both arch and imageIndex are set`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"children":[{"platform":"linux/amd64","manifestDigest":"sha256:0"}]}}]`:                                      `output x: image index child "linux/amd64": manifestDigest is not sha256:<64 hex>`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"children":[{"platform":"","manifestDigest":"` + d + `"}]}}]`:                                                `output x: platform "" is not os/arch`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"children":[{"platform":"linux riscv64","manifestDigest":"` + d + `"}]}}]`:                                   `output x: platform "linux riscv64" is not os/arch`,
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"requiredPlatforms":["linux/arm64"],"children":[{"platform":"linux/amd64","manifestDigest":"` + d + `"}]}}]`: `output x: imageIndex requires platform linux/arm64 but lists no child`,
		`"totalOutputs":2,"outputs":[{"name":"x","kind":"oci","digest":"` + d + `"}]`:                                                                                                        `totalOutputs is 2 but 1 outputs are listed`,
		`"inspectionDetails":{"outputSelection":"filtered"},"outputs":[{"name":"x","kind":"oci","digest":"` + d + `"}]`:                                                                      `the description is filtered to "filtered"`,
	}
	for body, want := range cases {
		if _, err := Parse([]byte(head + body + "}")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v\n%s", want, err, body)
		}
	}
	// A platform outside Archs is well formed: the output is simply for
	// another architecture, which Select reports for the output asked for.
	r, err := Parse([]byte(head + `"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","reference":"r.example/x@` + d + `","os":"linux","arch":"riscv64"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Select("x", "linux/amd64"); err == nil || !strings.Contains(err.Error(), "published only for other architectures: x (linux/riscv64)") {
		t.Errorf("riscv64 output: %v", err)
	}
}

// The no-* fixtures each lack one output the deploy path binds; the tagged
// one references its images as repository:tag@digest.
func TestLoad_FixturesLackingOneOutput(t *testing.T) {
	for file, missing := range map[string]string{
		"no-cwe.json": "cwe", "no-capnico.json": "capnico-controller", "no-controller-chart.json": "controller-chart",
		"no-nktp.json": "nktp", "no-profiles.json": "controller-profiles", "no-spire.json": "spire",
	} {
		r, err := Load(fixture(file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if _, err := r.Select(missing, "linux/arm64"); !errors.Is(err, ErrNotPublished) {
			t.Errorf("%s: Select(%s) = %v, want ErrNotPublished", file, missing, err)
		}
	}
	r, err := Load(fixture("tagged-capnico.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := r.Select("capnico-controller", "linux/arm64"); a.Repository() != "ghcr.io/example/recipes/capnico-controller-arm64" || !strings.Contains(a.Reference, ":v0.0.45@") {
		t.Errorf("tagged reference: %+v", a)
	}
}

func TestLoad_Valid(t *testing.T) {
	r, err := Load(fixture("valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.DeclaredQualification != "pipeline-attested" || len(r.Outputs) != 15 {
		t.Errorf("qualification %q outputs %d", r.DeclaredQualification, len(r.Outputs))
	}
	if !strings.HasSuffix(r.Reference, "/releases@sha256:"+strings.Repeat("0", 64)) || r.RootDigest != "sha256:"+strings.Repeat("0", 64) {
		t.Errorf("reference %s root %s", r.Reference, r.RootDigest)
	}
	if r.Metadata.ContentSubject != "sha256:"+strings.Repeat("c", 64) {
		t.Errorf("content subject: %q", r.Metadata.ContentSubject)
	}
	if charts := r.OfKind("helm-oci"); len(charts) != 6 || charts[0].Name != "controller-chart" || charts[0].ChartVersion != "0.1.1" {
		t.Errorf("helm-oci outputs: %+v", charts)
	}
	cwe, ok := r.Artifact("cwe")
	if !ok || cwe.Repository() != "ghcr.io/example/recipes/cwe" || cwe.Platform() != "" {
		t.Errorf("cwe: %+v %v", cwe, ok)
	}
	// The multi-arch image names its platforms through the index.
	if got := strings.Join(cwe.Platforms(), ","); got != "linux/amd64,linux/arm64" {
		t.Errorf("cwe platforms: %s", got)
	}
	if a, _ := r.Artifact("capnico-controller-arm64"); a.Platform() != "linux/arm64" || len(a.Platforms()) != 1 {
		t.Errorf("platform: %q %v", a.Platform(), a.Platforms())
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
	if _, err := nr.Select("cwe", "linux/arm64"); err == nil || !strings.Contains(err.Error(), "output cwe has no reference") {
		t.Errorf("no reference: %v", err)
	}
}

func TestSelect_ImageIndexNamesThePlatforms(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	amd := ImageIndexChild{Platform: "linux/amd64", ManifestDigest: "sha256:" + strings.Repeat("1", 64)}
	r := &Release{Outputs: []Artifact{
		{Name: "x", Kind: "oci", Digest: d, Reference: "r.example/x@" + d, ImageIndex: &ImageIndex{Children: []ImageIndexChild{amd}}},
	}}
	if got, err := r.Select("x", "linux/amd64"); err != nil || got.Name != "x" {
		t.Errorf("amd64: %q %v", got.Name, err)
	}
	// An index that lacks the architecture is not a fallback for it.
	if _, err := r.Select("x", "linux/arm64"); err == nil || !strings.Contains(err.Error(), "published only for other architectures: x (linux/amd64)") {
		t.Errorf("arm64: %v", err)
	}
}

// A child platform may carry a variant; the architecture is what is asked for.
func TestSelect_VariantIsTheArchitecture(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	r, err := Parse([]byte(`{"schemaVersion":"nvkr.dev/release-description/v1","reference":"r.example/x@` + d + `","rootDigest":"` + d + `",` +
		`"outputs":[{"name":"x","kind":"oci","digest":"` + d + `","reference":"r.example/x@` + d + `",` +
		`"imageIndex":{"requiredPlatforms":["linux/arm64"],"children":[{"platform":"linux/arm64/v8","manifestDigest":"` + d + `"}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x" {
		t.Errorf("arm64 via v8 child: %q %v", got.Name, err)
	}
	if _, err := r.Select("x", "linux/amd64"); err == nil || !strings.Contains(err.Error(), "published only for other architectures: x (linux/arm64/v8)") {
		t.Errorf("amd64: %v", err)
	}
}

func TestSelect_SuffixBeatsAnArchlessName(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	r := &Release{Outputs: []Artifact{
		{Name: "x", Kind: "oci", Digest: d, Reference: "r.example/x@" + d},
		{Name: "x-arm64", Kind: "oci", Digest: d, Reference: "r.example/x-arm64@" + d},
	}}
	// Without platforms the suffix is the only signal: the suffixed name is
	// the one built for arm64, and the plain name serves amd64.
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x-arm64" {
		t.Errorf("arm64: %q %v", got.Name, err)
	}
	if got, err := r.Select("x", "linux/amd64"); err != nil || got.Name != "x" {
		t.Errorf("amd64: %q %v", got.Name, err)
	}
	// A suffixed name asked for directly is still only for its architecture.
	if got, err := r.Select("x-arm64", "linux/arm64"); err != nil || got.Name != "x-arm64" {
		t.Errorf("direct arm64: %q %v", got.Name, err)
	}
	if _, err := r.Select("x-arm64", "linux/amd64"); err == nil || !strings.Contains(err.Error(), "published only for other architectures: x-arm64 (linux/arm64)") {
		t.Errorf("direct amd64: %v", err)
	}
	// With os and arch the name no longer matters.
	r.Outputs[0].OS, r.Outputs[0].Arch = "linux", "arm64"
	r.Outputs[1].OS, r.Outputs[1].Arch = "linux", "amd64"
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x" {
		t.Errorf("arch field wins: %q %v", got.Name, err)
	}
}

func TestSelect_Precedence(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	both := &ImageIndex{Children: []ImageIndexChild{{Platform: "linux/amd64", ManifestDigest: d}, {Platform: "linux/arm64", ManifestDigest: d}}}
	// The exact name covering arch beats a suffixed output that also covers it.
	r := &Release{Outputs: []Artifact{
		{Name: "x", Kind: "oci", Digest: d, Reference: "r.example/x@" + d, ImageIndex: both},
		{Name: "x-arm64", Kind: "oci", Digest: d, Reference: "r.example/x-arm64@" + d, OS: "linux", Arch: "arm64"},
	}}
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x" {
		t.Errorf("exact beats suffixed: %q %v", got.Name, err)
	}
	// Two suffixed outputs both covering arch: the one named for it wins,
	// whatever the document order.
	r = &Release{Outputs: []Artifact{
		{Name: "y-amd64", Kind: "oci", Digest: d, Reference: "r.example/y-amd64@" + d, ImageIndex: both},
		{Name: "y-arm64", Kind: "oci", Digest: d, Reference: "r.example/y-arm64@" + d, ImageIndex: both},
	}}
	if got, err := r.Select("y", "linux/amd64"); err != nil || got.Name != "y-amd64" {
		t.Errorf("suffix for arch wins: %q %v", got.Name, err)
	}
	if got, err := r.Select("y", "linux/arm64"); err != nil || got.Name != "y-arm64" {
		t.Errorf("suffix for arch wins: %q %v", got.Name, err)
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
