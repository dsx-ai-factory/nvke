// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package stackartifacts reads the stack artifacts file: the
// nvke.nvidia.com/stack-artifacts/v1 document that names every artifact the
// controller stack installs, by reference and digest. The deploy-path render
// and the bootstrap take their images and charts from it.
//
//	{
//	  "schemaVersion": "nvke.nvidia.com/stack-artifacts/v1",
//	  "source": {"reference": "<registry>/<repo>/releases@sha256:<64 hex>", "declaredQualification": "pipeline-attested"},
//	  "artifacts": [
//	    {"name": "cwe", "kind": "oci", "digest": "sha256:<64 hex>",
//	     "reference": "<registry>/<repo>@sha256:<64 hex>",
//	     "os": "linux", "arch": "arm64", "layout": "images/cwe"},
//	    {"name": "controller-chart", "kind": "helm-oci", "chartVersion": "0.4.1", ...},
//	    {"name": "multi", "kind": "oci", "imageIndex": {"children": [{"platform": "linux/amd64", ...}]}, ...}
//	  ]
//	}
//
// The file is written by an operator, by make local-stack-artifacts, or by
// hack/nvkr-run-to-stack-artifacts.py from an NVKR release export. source is
// informational: it says where the artifacts came from, not that they were
// verified, and nothing here verifies it.
//
// What the file requires: the schema version, a non-empty artifact list with
// unique names, and for every artifact a name, a kind and a sha256 digest,
// which may come from the reference. A reference must be an OCI reference
// ending in that digest; a platform, native or in an image index, is os/arch
// with an optional variant. A layout is a directory holding an OCI image
// layout whose index lists the digest; it is relative to the file unless
// absolute, and with it nothing is pulled from the reference's host: the
// reference is the name the image is stored under. Unknown fields are refused.
//
// Artifact names are the names the deploy path binds: each item's release.yaml
// names the artifact it uses (`artifact:`, `chart.artifact`,
// `chart.imageArtifact`, `chart.profilesArtifact`), so no mapping lives in
// code. Select resolves a name for one architecture: a native artifact
// carries os and arch; a multi-arch image carries an imageIndex that lists
// its platforms; per-architecture artifacts may also be published as `<name>`
// and `<name>-arm64`.
package stackartifacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// SchemaVersion is the stack artifacts schema this package reads.
const SchemaVersion = "nvke.nvidia.com/stack-artifacts/v1"

// nvkrSchemaPrefix marks the document `nvkr release export` writes, which
// hack/nvkr-run-to-stack-artifacts.py converts.
const nvkrSchemaPrefix = "nvkr.dev/release-description/"

// Archs lists the architectures Select accepts, as os/arch.
var Archs = []string{"linux/amd64", "linux/arm64"}

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// refPattern is the OCI distribution reference grammar with the host
	// required: a dotted name, localhost, a bracketed IPv6 address, each with
	// an optional port, or any name with a port; then one or more lowercase
	// path components, an optional tag, and a sha256 digest. A first
	// component without a dot or a port is a namespace, not a registry, and
	// is refused: every reference is registry-qualified.
	refPattern = regexp.MustCompile(`^(?:(?:[A-Za-z0-9]+(?:-+[A-Za-z0-9]+)*(?:\.[A-Za-z0-9]+(?:-+[A-Za-z0-9]+)*)+|localhost|\[[0-9A-Fa-f:.]+\])(?::[0-9]+)?|[A-Za-z0-9]+(?:-+[A-Za-z0-9]+)*:[0-9]+)` +
		`(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}$`)
	tokenPattern    = regexp.MustCompile(`^[\w.-]+$`)
	versionPattern  = regexp.MustCompile(`^[\w.+-]+$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(?:/[a-z0-9]+)?$`)
)

// Set is the stack artifacts file: the set of artifacts the stack installs.
type Set struct {
	SchemaVersion string `json:"schemaVersion"`
	// Source says where the artifacts came from. Optional and informational.
	Source *Source `json:"source,omitempty"`
	// Artifacts are the artifacts, in document order.
	Artifacts []Artifact `json:"artifacts"`
}

// Source is the publication the artifacts were taken from: an NVKR release
// reference and the qualification its publisher declared.
type Source struct {
	Reference             string `json:"reference"`
	DeclaredQualification string `json:"declaredQualification,omitempty"`
}

// Artifact is one artifact the stack installs.
type Artifact struct {
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Digest       string      `json:"digest"`
	Reference    string      `json:"reference,omitempty"`
	OS           string      `json:"os,omitempty"`
	Arch         string      `json:"arch,omitempty"`
	ChartVersion string      `json:"chartVersion,omitempty"`
	ImageIndex   *ImageIndex `json:"imageIndex,omitempty"`
	// Layout is the OCI image layout that holds the artifact, when it is
	// local. Load makes it absolute.
	Layout string `json:"layout,omitempty"`
}

// ImageIndex describes a multi-arch image: the platforms it was required to
// cover and the manifest published for each.
type ImageIndex struct {
	RequiredPlatforms []string          `json:"requiredPlatforms,omitempty"`
	Children          []ImageIndexChild `json:"children"`
}

// ImageIndexChild is one platform's manifest inside an image index.
type ImageIndexChild struct {
	Platform       string `json:"platform"`
	ManifestDigest string `json:"manifestDigest"`
}

// Repository returns the reference without its digest and without a tag.
func (a Artifact) Repository() string {
	repo, _, _ := strings.Cut(a.Reference, "@")
	if slash := strings.LastIndex(repo, "/"); strings.Contains(repo[slash+1:], ":") {
		repo = repo[:slash+1+strings.Index(repo[slash+1:], ":")]
	}
	return repo
}

// Host returns the registry host of the reference, with its port, or ""
// without a reference.
func (a Artifact) Host() string {
	host, _, _ := strings.Cut(a.Reference, "/")
	return host
}

// Platform returns a native artifact's platform as os/arch, or "" when the
// artifact names none: a multi-arch index, or a kind without one.
func (a Artifact) Platform() string {
	if a.Arch == "" {
		return ""
	}
	if strings.Contains(a.Arch, "/") {
		return a.Arch
	}
	os := a.OS
	if os == "" {
		os = "linux"
	}
	return os + "/" + a.Arch
}

// Platforms returns the platforms the artifact is published for: the native
// platform, or the children of its image index. Empty means the artifact
// names none and serves any architecture.
func (a Artifact) Platforms() []string {
	if p := a.Platform(); p != "" {
		return []string{p}
	}
	if a.ImageIndex == nil {
		return nil
	}
	var out []string
	for _, c := range a.ImageIndex.Children {
		out = append(out, c.Platform)
	}
	return out
}

// PublishedPlatforms returns the platforms an artifact is published for as
// selection reads them: Platforms, or for an artifact that names none, the
// architecture its name's -amd64 or -arm64 suffix says. Empty means the
// artifact serves any architecture.
func (a Artifact) PublishedPlatforms() []string {
	if platforms := a.Platforms(); len(platforms) > 0 {
		return platforms
	}
	if own := archSuffix(a.Name); own != "" {
		return []string{own}
	}
	return nil
}

// Load reads and validates the file at path and makes every relative layout
// absolute against the file's directory. Errors name the path. The layouts
// are not read: CheckLayouts does that.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)
	for i := range r.Artifacts {
		if layout := r.Artifacts[i].Layout; layout != "" && !filepath.IsAbs(layout) {
			r.Artifacts[i].Layout = filepath.Join(dir, layout)
		}
	}
	return r, nil
}

// Parse decodes and validates a stack artifacts document. Layouts are left
// as written.
func Parse(data []byte) (*Set, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("the file is empty")
	}
	// The schema id first, loosely, so a document of another schema is
	// named for what it is rather than for its first unknown field.
	var head struct {
		SchemaVersion string `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("not a JSON document: %w", err)
	}
	if strings.HasPrefix(head.SchemaVersion, nvkrSchemaPrefix) {
		return nil, fmt.Errorf("schemaVersion is %q: this is an NVKR release description; convert it with hack/nvkr-run-to-stack-artifacts.py", head.SchemaVersion)
	}
	if head.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schemaVersion is %q, expected %s", head.SchemaVersion, SchemaVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Set
	if err := dec.Decode(&r); err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "json: "))
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Marshal writes the document as JSON, indented, with the schema version
// set. A Set built in code and one read from a file write the same.
func (r *Set) Marshal() ([]byte, error) {
	doc := *r
	doc.SchemaVersion = SchemaVersion
	data, err := json.MarshalIndent(&doc, "", " ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (r *Set) validate() error {
	if r.Source != nil {
		if r.Source.Reference == "" {
			// An empty block, or one without a reference, says nothing.
			r.Source = nil
		} else {
			if !refPattern.MatchString(r.Source.Reference) {
				return fmt.Errorf("source.reference is not an OCI reference with a sha256 digest: %s", r.Source.Reference)
			}
			if q := r.Source.DeclaredQualification; q != "" && !tokenPattern.MatchString(q) {
				return fmt.Errorf("source.declaredQualification must be a token: %q", q)
			}
		}
	}
	if len(r.Artifacts) == 0 {
		return errors.New("artifacts is empty")
	}
	names := make(map[string]bool, len(r.Artifacts))
	for i := range r.Artifacts {
		a := &r.Artifacts[i]
		if err := a.validate(i); err != nil {
			return err
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate artifact name: %s", a.Name)
		}
		names[a.Name] = true
	}
	return nil
}

func (a *Artifact) validate(i int) error {
	if a.Name == "" || a.Kind == "" {
		return fmt.Errorf("artifacts[%d]: name and kind are required", i)
	}
	// The example file's placeholders are refused by name, as the stack
	// config's are, so a copied example fails on the first unfilled field.
	for _, f := range []struct{ field, value string }{{"name", a.Name}, {"kind", a.Kind}, {"digest", a.Digest}, {"reference", a.Reference}, {"chartVersion", a.ChartVersion}, {"os", a.OS}, {"arch", a.Arch}, {"layout", a.Layout}} {
		if strings.Contains(f.value, "REPLACE_") {
			return fmt.Errorf("artifact %s: %s is still a REPLACE_* placeholder; fill it in", a.Name, f.field)
		}
	}
	if !tokenPattern.MatchString(a.Name) {
		return fmt.Errorf("artifact name must be a token: %q", a.Name)
	}
	if !tokenPattern.MatchString(a.Kind) {
		return fmt.Errorf("artifact %s: kind must be a token: %q", a.Name, a.Kind)
	}
	if a.ChartVersion != "" && !versionPattern.MatchString(a.ChartVersion) {
		return fmt.Errorf("artifact %s: chartVersion is not a version: %q", a.Name, a.ChartVersion)
	}
	if a.Reference != "" && !refPattern.MatchString(a.Reference) {
		return fmt.Errorf("artifact %s: reference is not an OCI reference with a sha256 digest: %s", a.Name, a.Reference)
	}
	switch {
	case a.Digest == "" && a.Reference == "":
		return fmt.Errorf("artifact %s: a digest or a reference carrying one is required", a.Name)
	case a.Digest == "":
		a.Digest = a.Reference[strings.LastIndex(a.Reference, "@")+1:]
	case !digestPattern.MatchString(a.Digest):
		return fmt.Errorf("artifact %s: digest is not sha256:<64 hex>: %s", a.Name, a.Digest)
	case a.Reference != "" && !strings.HasSuffix(a.Reference, "@"+a.Digest):
		return fmt.Errorf("artifact %s: reference %s does not end in its digest %s", a.Name, a.Reference, a.Digest)
	}
	if a.OS != "" && a.Arch == "" {
		return fmt.Errorf("artifact %s: os %q without arch", a.Name, a.OS)
	}
	if a.OS != "" && strings.Contains(a.Arch, "/") && !strings.HasPrefix(a.Arch, a.OS+"/") {
		return fmt.Errorf("artifact %s: os %q disagrees with arch %q", a.Name, a.OS, a.Arch)
	}
	if a.Arch != "" && a.ImageIndex != nil {
		return fmt.Errorf("artifact %s: both arch and imageIndex are set; a native artifact has one platform, an index lists its children", a.Name)
	}
	if a.ImageIndex != nil {
		if len(a.ImageIndex.Children) == 0 {
			return fmt.Errorf("artifact %s: imageIndex has no children", a.Name)
		}
		for _, c := range a.ImageIndex.Children {
			if !digestPattern.MatchString(c.ManifestDigest) {
				return fmt.Errorf("artifact %s: image index child %q: manifestDigest is not sha256:<64 hex>: %q", a.Name, c.Platform, c.ManifestDigest)
			}
		}
		for _, p := range a.ImageIndex.RequiredPlatforms {
			if !coversPlatform(a.Platforms(), p) {
				return fmt.Errorf("artifact %s: imageIndex requires platform %s but lists no child for it", a.Name, p)
			}
		}
	}
	for _, p := range a.Platforms() {
		if !platformPattern.MatchString(p) {
			return fmt.Errorf("artifact %s: platform %q is not os/arch", a.Name, p)
		}
	}
	return nil
}

// CheckLayouts reads every artifact's layout and returns one error per
// layout that is missing, unreadable, or does not list the artifact's
// digest. A layout path is read as given: call it on a Set from Load,
// which made the paths absolute.
func (r *Set) CheckLayouts() []error {
	var errs []error
	for _, a := range r.Artifacts {
		if a.Layout == "" {
			continue
		}
		if err := layoutLists(a.Layout, a.Digest); err != nil {
			errs = append(errs, fmt.Errorf("artifact %s: %w", a.Name, err))
		}
	}
	return errs
}

// layoutLists reports whether the OCI layout at dir lists a manifest with
// digest in its index.
func layoutLists(dir, digest string) error {
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return fmt.Errorf("layout %s: %w", dir, err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("layout %s: index.json: %w", dir, err)
	}
	for _, manifest := range index.Manifests {
		if manifest.Digest.String() == digest {
			return nil
		}
	}
	return fmt.Errorf("layout %s does not list a manifest with digest %s", dir, digest)
}

// Artifact returns the artifact of that exact name.
func (r *Set) Artifact(name string) (Artifact, bool) {
	for _, a := range r.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// OfKind returns the artifacts of one kind, in document order. An empty
// kind returns every artifact.
func (r *Set) OfKind(kind string) []Artifact {
	var out []Artifact
	for _, a := range r.Artifacts {
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// Candidates returns the artifacts Select considers for name, whatever the
// architecture: the one of that exact name and the ones named
// <name>-<arch suffix>.
func (r *Set) Candidates(name string) []Artifact {
	var out []Artifact
	for _, a := range r.Artifacts {
		if a.Name == name || contains(Siblings(name), a.Name) {
			out = append(out, a)
		}
	}
	return out
}

// Siblings returns the per-architecture names of name: <name>-amd64 and
// <name>-arm64.
func Siblings(name string) []string {
	var out []string
	for _, arch := range Archs {
		out = append(out, name+"-"+arch[strings.Index(arch, "/")+1:])
	}
	return out
}

// Kinds of the artifacts an item binds: KindImage for an image role,
// KindChart for the chart role.
const (
	KindImage = "oci"
	KindChart = "helm-oci"
)

// ErrOtherArch reports that artifacts of the name exist, none for the
// architecture asked for.
var ErrOtherArch = errors.New("published only for other architectures")

// ErrNotProvided reports that no artifact of the name exists for any
// architecture. A caller that has a fallback checks for it.
var ErrNotProvided = errors.New("not in the stack artifacts")

// archSuffix returns the architecture a name's `-<arch>` suffix names, in
// the form Archs uses, or "".
func archSuffix(name string) string {
	for _, candidate := range Archs {
		if strings.HasSuffix(name, "-"+candidate[strings.Index(candidate, "/")+1:]) {
			return candidate
		}
	}
	return ""
}

// Select returns the artifact named name for arch (linux/amd64 or
// linux/arm64). Candidates are the artifact of that exact name and the ones
// named <name>-<arch suffix> for any architecture. An artifact that names
// its platforms, natively or through its image index, is taken only when
// they include arch; one that names none counts as built for the
// architecture its own name's suffix says, else for any. The exact name for
// arch wins, then the suffixed name for arch, then the exact name naming no
// platform. No artifact of the name at all is ErrNotProvided; artifacts
// only for other architectures, or a candidate without a reference, are
// errors that say so.
func (r *Set) Select(name, arch string) (Artifact, error) {
	if !contains(Archs, arch) {
		return Artifact{}, fmt.Errorf("arch %s is not one of %s", arch, strings.Join(Archs, ", "))
	}
	var exact, suffixed, anyArch *Artifact
	var others []string
	for i := range r.Artifacts {
		a := &r.Artifacts[i]
		bySuffix := ""
		if a.Name != name {
			bySuffix = archSuffix(a.Name)
			if bySuffix == "" || a.Name != name+"-"+bySuffix[strings.Index(bySuffix, "/")+1:] {
				continue
			}
		}
		platforms := a.PublishedPlatforms()
		switch {
		case len(platforms) == 0:
			anyArch = a
		case coversPlatform(platforms, arch) && a.Name == name:
			exact = a
		case coversPlatform(platforms, arch):
			// Two suffixed artifacts may both cover arch; the one named for it wins.
			if suffixed == nil || bySuffix == arch {
				suffixed = a
			}
		default:
			others = append(others, a.Name+" ("+strings.Join(platforms, ", ")+")")
		}
	}
	chosen := exact
	if chosen == nil {
		chosen = suffixed
	}
	if chosen == nil {
		chosen = anyArch
	}
	if chosen == nil {
		if len(others) > 0 {
			return Artifact{}, fmt.Errorf("%s is %w: %s", name, ErrOtherArch, strings.Join(others, ", "))
		}
		return Artifact{}, fmt.Errorf("%s: %w", name, ErrNotProvided)
	}
	if chosen.Reference == "" {
		return Artifact{}, fmt.Errorf("artifact %s has no reference; the stack artifacts file must say where it is pulled from, or stored under", chosen.Name)
	}
	return *chosen, nil
}

// SelectKind is Select for the artifact an item binds in one role: an
// artifact of another kind than the role takes, KindImage or KindChart, is
// an error naming both kinds.
func (r *Set) SelectKind(name, kind, arch string) (Artifact, error) {
	a, err := r.Select(name, arch)
	if err != nil {
		return Artifact{}, err
	}
	if a.Kind != kind {
		return Artifact{}, fmt.Errorf("artifact %s is kind %s, not %s", a.Name, a.Kind, kind)
	}
	return a, nil
}

// osArch returns a platform without its variant: linux/arm64/v8 is linux/arm64.
func osArch(platform string) string {
	if i := strings.Index(platform, "/"); i >= 0 {
		if j := strings.Index(platform[i+1:], "/"); j >= 0 {
			return platform[:i+1+j]
		}
	}
	return platform
}

// coversPlatform reports whether one of platforms is want, variants aside.
func coversPlatform(platforms []string, want string) bool {
	for _, p := range platforms {
		if osArch(p) == osArch(want) {
			return true
		}
	}
	return false
}

func contains(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}
