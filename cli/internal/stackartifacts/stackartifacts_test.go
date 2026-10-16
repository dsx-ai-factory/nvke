// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackartifacts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(name string) string {
	return filepath.Join("..", "..", "..", "deploy", "testdata", "stack-artifacts", name)
}

// The old-schema export, kept for the converter's tests.
func nvkrFixture(name string) string {
	return filepath.Join("..", "..", "..", "hack", "testdata", "nvkr-release", name)
}

func TestLoad_BadFixturesNameTheProblem(t *testing.T) {
	cases := map[string]string{
		"bad-schema.json":                   `schemaVersion is "nvke.nvidia.com/stack-artifacts/v2", expected nvke.nvidia.com/stack-artifacts/v1`,
		"verification-result.json":          `schemaVersion is "nvkr.dev/release-verification-result/v1", expected`,
		"nvkr-export-refused.json":          `schemaVersion is "nvkr.dev/release-description/v1": this is an NVKR release description; convert it with hack/nvkr-run-to-stack-artifacts.py`,
		"outputs-key.json":                  `unknown field "outputs"`,
		"unknown-field.json":                `unknown field "rootDigest"`,
		"unknown-artifact-field.json":       `unknown field "buildDigest"`,
		"empty-index.json":                  "artifact cwe: imageIndex has no children",
		"neither-digest-nor-reference.json": "artifact cwe: a digest or a reference carrying one is required",
		"digest-reference-disagree.json":    "does not end in its digest",
		"bad-digest.json":                   "artifact cwe: digest is not sha256:<64 hex>",
		"bad-reference.json":                "artifact cwe: reference is not an OCI reference",
		"reference-digest-mismatch.json":    "does not end in its digest",
		"duplicate-name.json":               "duplicate artifact name: cwe",
		"bad-arch.json":                     `artifact capnico-controller: platform "linux/x86 64" is not os/arch`,
		"no-artifacts.json":                 "artifacts is empty",
		"not-json.json":                     "not a JSON document",
		"source-bad-reference.json":         "source.reference is not an OCI reference with a sha256 digest: releases",
		"source-bad-qualification.json":     `source.declaredQualification must be a token: "pipeline attested"`,
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
	// The real export, in the shape nvkr release export writes, gets the
	// converter hint and not its first unknown field.
	_, err := Load(nvkrFixture("valid.json"))
	if err == nil || !strings.Contains(err.Error(), "convert it with hack/nvkr-run-to-stack-artifacts.py") || strings.Contains(err.Error(), "unknown field") {
		t.Errorf("nvkr export: %v", err)
	}
}

func TestParse_Source(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	ref := "ghcr.io/example/recipes/releases@" + d
	for file, want := range map[string]*Source{
		"source-empty.json":        nil,
		"source-null.json":         nil,
		"source-no-reference.json": nil,
		"valid.json":               {Reference: "ghcr.io/example/recipes/releases@sha256:" + strings.Repeat("0", 64), DeclaredQualification: "pipeline-attested"},
	} {
		r, err := Load(fixture(file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		switch {
		case want == nil && r.Source != nil:
			t.Errorf("%s: source %+v, want none", file, r.Source)
		case want != nil && (r.Source == nil || *r.Source != *want):
			t.Errorf("%s: source %+v, want %+v", file, r.Source, want)
		}
	}
	// A reference without a qualification is a source.
	r, err := Parse([]byte(`{"schemaVersion":"` + SchemaVersion + `","source":{"reference":"` + ref + `"},"artifacts":[{"name":"cwe","kind":"oci","digest":"` + d + `"}]}`))
	if err != nil || r.Source == nil || r.Source.Reference != ref || r.Source.DeclaredQualification != "" {
		t.Errorf("source without qualification: %+v %v", r.Source, err)
	}
}

func TestParse_DigestFromReference(t *testing.T) {
	r, err := Load(fixture("digest-from-reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	cwe, _ := r.Artifact("cwe")
	if cwe.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("digest not taken from the reference: %+v", cwe)
	}
	// Both present and equal is fine; the file says it twice.
	if _, err := Load(fixture("valid.json")); err != nil {
		t.Error(err)
	}
}

func TestParse_RefusesMalformedArtifacts(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	head := `{"schemaVersion":"` + SchemaVersion + `",`
	cases := map[string]string{
		`"artifacts":[{"name":"a b","kind":"oci","digest":"` + d + `"}]`:                                                                                                                       `artifact name must be a token: "a b"`,
		`"artifacts":[{"name":"x","digest":"` + d + `"}]`:                                                                                                                                      `artifacts[0]: name and kind are required`,
		`"artifacts":[{"name":"x","kind":"helm oci","digest":"` + d + `"}]`:                                                                                                                    `artifact x: kind must be a token`,
		`"artifacts":[{"name":"x","kind":"helm-oci","digest":"` + d + `","chartVersion":"1.0 beta"}]`:                                                                                          `artifact x: chartVersion is not a version`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","reference":"not-a-ref@` + d + `"}]`:                                                                                       `artifact x: reference is not an OCI reference`,
		`"artifacts":[{"name":"x","kind":"oci","reference":"not-a-ref@` + d + `"}]`:                                                                                                            `artifact x: reference is not an OCI reference`,
		`"artifacts":[{"name":"x","kind":"oci","reference":"myorg/cwe@` + d + `"}]`:                                                                                                            `artifact x: reference is not an OCI reference`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","os":"linux"}]`:                                                                                                            `artifact x: os "linux" without arch`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","os":"windows","arch":"linux/amd64"}]`:                                                                                     `artifact x: os "windows" disagrees with arch`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","arch":"amd64","imageIndex":{"children":[{"platform":"linux/amd64","manifestDigest":"` + d + `"}]}}]`:                      `artifact x: both arch and imageIndex are set`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"children":[{"platform":"linux/amd64","manifestDigest":"sha256:0"}]}}]`:                                      `artifact x: image index child "linux/amd64": manifestDigest is not sha256:<64 hex>`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"children":[{"platform":"","manifestDigest":"` + d + `"}]}}]`:                                                `artifact x: platform "" is not os/arch`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"children":[{"platform":"linux riscv64","manifestDigest":"` + d + `"}]}}]`:                                   `artifact x: platform "linux riscv64" is not os/arch`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","imageIndex":{"requiredPlatforms":["linux/arm64"],"children":[{"platform":"linux/amd64","manifestDigest":"` + d + `"}]}}]`: `artifact x: imageIndex requires platform linux/arm64 but lists no child`,
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","nvkr":true}]`:                                                                                                             `unknown field "nvkr"`,
		`"source":{"reference":"` + d + `"},"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `"}]`:                                                                                      `source.reference is not an OCI reference`,
	}
	for body, want := range cases {
		if _, err := Parse([]byte(head + body + "}")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v\n%s", want, err, body)
		}
	}
	// A registry is a dotted name, localhost or any name with a port.
	for _, host := range []string{"nvke.local", "localhost", "localhost:5000", "registry:5000", "ghcr.io", "[::1]:5000", "127.0.0.1:5000"} {
		if _, err := Parse([]byte(head + `"artifacts":[{"name":"x","kind":"oci","reference":"` + host + `/ns/x@` + d + `"}]}`)); err != nil {
			t.Errorf("host %s refused: %v", host, err)
		}
	}
	// A platform outside Archs is well formed: the artifact is simply for
	// another architecture, which Select reports for the artifact asked for.
	r, err := Parse([]byte(head + `"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","reference":"r.example/x@` + d + `","os":"linux","arch":"riscv64"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Select("x", "linux/amd64"); !errors.Is(err, ErrOtherArch) || !strings.Contains(err.Error(), "published only for other architectures: x (linux/riscv64)") {
		t.Errorf("riscv64 artifact: %v", err)
	}
	// SelectKind binds an artifact only in the role its kind takes.
	r, err = Parse([]byte(head + `"artifacts":[{"name":"y","kind":"oci","digest":"` + d + `","reference":"r.example/y@` + d + `","os":"linux","arch":"amd64"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a, err := r.SelectKind("y", KindImage, "linux/amd64"); err != nil || a.Name != "y" {
		t.Errorf("SelectKind for an image: %v %v", a, err)
	}
	if _, err := r.SelectKind("y", KindChart, "linux/amd64"); err == nil || err.Error() != "artifact y is kind oci, not helm-oci" {
		t.Errorf("SelectKind of the wrong kind: %v", err)
	}
	// The example file is refused by its first placeholder, by field name.
	if _, err := Load(filepath.Join("..", "..", "..", "deploy", "examples", "stack-artifacts.json")); err == nil || !strings.Contains(err.Error(), "is still a REPLACE_* placeholder; fill it in") {
		t.Errorf("example with placeholders: %v", err)
	}
}

// The no-* fixtures each lack one artifact the deploy path binds; the tagged
// one references its images as repository:tag@digest.
func TestLoad_FixturesLackingOneArtifact(t *testing.T) {
	for file, missing := range map[string]string{
		"no-cwe.json": "cwe", "no-capnico.json": "capnico-controller", "no-controller-chart.json": "controller-chart",
		"no-nktp.json": "nktp", "no-profiles.json": "controller-profiles", "no-spire.json": "spire",
	} {
		r, err := Load(fixture(file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if _, err := r.Select(missing, "linux/arm64"); !errors.Is(err, ErrNotProvided) {
			t.Errorf("%s: Select(%s) = %v, want ErrNotProvided", file, missing, err)
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
	if r.Source == nil || r.Source.DeclaredQualification != "pipeline-attested" || len(r.Artifacts) != 15 {
		t.Errorf("source %+v artifacts %d", r.Source, len(r.Artifacts))
	}
	if charts := r.OfKind("helm-oci"); len(charts) != 6 || charts[0].Name != "controller-chart" || charts[0].ChartVersion != "0.1.1" {
		t.Errorf("helm-oci artifacts: %+v", charts)
	}
	cwe, ok := r.Artifact("cwe")
	if !ok || cwe.Repository() != "ghcr.io/example/recipes/cwe" || cwe.Platform() != "" || cwe.Host() != "ghcr.io" || cwe.Layout != "" {
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
	if tagged.Repository() != "registry.example.test:5000/nvke/cwe" || tagged.Host() != "registry.example.test:5000" {
		t.Errorf("repository drops the tag: %s %s", tagged.Repository(), tagged.Host())
	}
	if (Artifact{}).Host() != "" {
		t.Error("host without a reference")
	}
}

func TestMarshal_RoundTrips(t *testing.T) {
	r, err := Load(fixture("valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if len(again.Artifacts) != len(r.Artifacts) || *again.Source != *r.Source || again.Artifacts[0].ImageIndex == nil {
		t.Errorf("round trip changed the document:\n%s", data)
	}
	// A Set built in code writes the schema version and no empty fields.
	built := &Set{Artifacts: []Artifact{{Name: "x", Kind: "oci", Digest: "sha256:" + strings.Repeat("a", 64)}}}
	data, err = built.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schemaVersion": "`+SchemaVersion+`"`) || strings.Contains(string(data), `"source"`) || strings.Contains(string(data), `"layout"`) || strings.Contains(string(data), `"reference"`) {
		t.Errorf("built document:\n%s", data)
	}
	if _, err := Parse(data); err != nil {
		t.Errorf("built document does not load: %v", err)
	}
}

func TestLoad_Layouts(t *testing.T) {
	dir, err := filepath.Abs(filepath.Dir(fixture("layout-ok.json")))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Load(fixture("layout-ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	cwe, _ := r.Artifact("cwe")
	if cwe.Layout != filepath.Join(dir, "layouts", "cwe") {
		t.Errorf("relative layout not made absolute against the file: %s", cwe.Layout)
	}
	if errs := r.CheckLayouts(); len(errs) != 0 {
		t.Errorf("layout-ok: %v", errs)
	}
	// Parse leaves the path as written.
	data, _ := os.ReadFile(fixture("layout-ok.json"))
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Artifacts[0].Layout != "layouts/cwe" {
		t.Errorf("Parse changed the layout: %s", parsed.Artifacts[0].Layout)
	}
	// An absolute layout is kept.
	r, err = Load(fixture("layout-absolute.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Artifacts[0].Layout != "/nonexistent/layouts/cwe" {
		t.Errorf("absolute layout changed: %s", r.Artifacts[0].Layout)
	}
	if errs := r.CheckLayouts(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "artifact cwe: layout /nonexistent/layouts/cwe:") {
		t.Errorf("absent absolute layout: %v", errs)
	}
	for file, want := range map[string]string{
		"layout-missing.json":        "artifact cwe: layout " + filepath.Join(dir, "layouts", "absent") + ":",
		"layout-wrong-digest.json":   "does not list a manifest with digest sha256:" + strings.Repeat("b", 64),
		"layout-index-not-json.json": "index.json:",
	} {
		r, err := Load(fixture(file))
		if err != nil {
			t.Fatalf("%s: Load refused a layout problem, which CheckLayouts reports: %v", file, err)
		}
		errs := r.CheckLayouts()
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), want) {
			t.Errorf("%s: want %q, got %v", file, want, errs)
		}
	}
	// Several bad layouts are all reported.
	r = &Set{Artifacts: []Artifact{
		{Name: "a", Kind: "oci", Digest: "sha256:" + strings.Repeat("a", 64), Layout: filepath.Join(t.TempDir(), "a")},
		{Name: "b", Kind: "oci", Digest: "sha256:" + strings.Repeat("b", 64), Layout: filepath.Join(t.TempDir(), "b")},
		{Name: "c", Kind: "oci", Digest: "sha256:" + strings.Repeat("c", 64)},
	}}
	if errs := r.CheckLayouts(); len(errs) != 2 {
		t.Errorf("two missing layouts: %v", errs)
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
		{"absent", "linux/arm64", "", "absent: not in the stack artifacts"},
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
	if _, err := r.Select("absent", "linux/arm64"); !errors.Is(err, ErrNotProvided) {
		t.Errorf("absent is not ErrNotProvided: %v", err)
	}
	nr, err := Load(fixture("no-reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nr.Select("cwe", "linux/arm64"); err == nil || !strings.Contains(err.Error(), "artifact cwe has no reference") {
		t.Errorf("no reference: %v", err)
	}
}

func TestCandidatesAndSiblings(t *testing.T) {
	r, err := Load(fixture("valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Siblings("x"); strings.Join(got, ",") != "x-amd64,x-arm64" {
		t.Errorf("siblings: %v", got)
	}
	names := func(as []Artifact) string {
		var out []string
		for _, a := range as {
			out = append(out, a.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(r.Candidates("capnico-controller")); got != "capnico-controller,capnico-controller-arm64" {
		t.Errorf("candidates: %s", got)
	}
	if got := names(r.Candidates("cwe")); got != "cwe" {
		t.Errorf("candidates: %s", got)
	}
	if got := r.Candidates("absent"); len(got) != 0 {
		t.Errorf("candidates of an absent name: %v", got)
	}
}

func TestSelect_ImageIndexNamesThePlatforms(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	amd := ImageIndexChild{Platform: "linux/amd64", ManifestDigest: "sha256:" + strings.Repeat("1", 64)}
	r := &Set{Artifacts: []Artifact{
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
	r, err := Parse([]byte(`{"schemaVersion":"` + SchemaVersion + `",` +
		`"artifacts":[{"name":"x","kind":"oci","digest":"` + d + `","reference":"r.example/x@` + d + `",` +
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
	r := &Set{Artifacts: []Artifact{
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
	r.Artifacts[0].OS, r.Artifacts[0].Arch = "linux", "arm64"
	r.Artifacts[1].OS, r.Artifacts[1].Arch = "linux", "amd64"
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x" {
		t.Errorf("arch field wins: %q %v", got.Name, err)
	}
}

func TestSelect_Precedence(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	both := &ImageIndex{Children: []ImageIndexChild{{Platform: "linux/amd64", ManifestDigest: d}, {Platform: "linux/arm64", ManifestDigest: d}}}
	// The exact name covering arch beats a suffixed artifact that also covers it.
	r := &Set{Artifacts: []Artifact{
		{Name: "x", Kind: "oci", Digest: d, Reference: "r.example/x@" + d, ImageIndex: both},
		{Name: "x-arm64", Kind: "oci", Digest: d, Reference: "r.example/x-arm64@" + d, OS: "linux", Arch: "arm64"},
	}}
	if got, err := r.Select("x", "linux/arm64"); err != nil || got.Name != "x" {
		t.Errorf("exact beats suffixed: %q %v", got.Name, err)
	}
	// Two suffixed artifacts both covering arch: the one named for it wins,
	// whatever the document order.
	r = &Set{Artifacts: []Artifact{
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
