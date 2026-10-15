// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvkrrelease reads the release description NVKR exports: the
// nvkr.dev/release-description/v1 document `nvkr release export` writes for
// one published release. The deploy-path render takes every artifact it
// pulls from it, by reference and digest.
//
//	{
//	  "schemaVersion": "nvkr.dev/release-description/v1",
//	  "reference": "<registry>/<repo>/releases@sha256:<64 hex>",
//	  "rootDigest": "sha256:<64 hex>",
//	  "declaredQualification": "pipeline-attested",
//	  "inspectionDetails": {"outputSelection": "all", ...},
//	  "totalOutputs": 3,
//	  "metadata": {"contentSubject": "sha256:<64 hex>", ...},
//	  "outputs": [
//	    {"name": "cwe", "kind": "oci", "digest": "sha256:<64 hex>",
//	     "reference": "<registry>/<repo>@sha256:<64 hex>",
//	     "os": "linux", "arch": "arm64", ...},
//	    {"name": "controller-chart", "kind": "helm-oci", "chartVersion": "0.4.1", ...},
//	    {"name": "multi", "kind": "oci", "imageIndex": {"children": [{"platform": "linux/amd64", ...}]}, ...}
//	  ],
//	  "evidence": [...]
//	}
//
// The description is metadata only. It says what the release holds, not that
// it was verified, and nothing here verifies it: the operator runs
// `nvkr release verify` on the same reference first. A filtered description,
// as `nvkr release browse --output` writes, is refused.
//
// The document is NVKR's, so fields this package does not read are ignored.
// What it requires: the schema version, a reference ending in the root
// digest, a complete output list, and for every output a name, a kind and a
// sha256 digest; a reference, when present, must be an OCI reference ending
// in that digest; a platform, native or in an image index, is os/arch with
// an optional variant. Names are unique.
//
// Output names are NVKR's. The deploy path binds each item to the output it
// uses by that name (`artifact:` in the item's release.yaml), so no mapping
// lives in code. Select resolves a name for one architecture: a native
// output carries os and arch; a multi-arch image carries an imageIndex that
// lists its platforms; NVKR may also publish per-architecture outputs as
// `<name>` and `<name>-arm64`.
package nvkrrelease

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SchemaVersion is the release description schema this package reads.
const SchemaVersion = "nvkr.dev/release-description/v1"

// Archs lists the architectures Select accepts, in the form NVKR uses.
var Archs = []string{"linux/amd64", "linux/arm64"}

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// refPattern is the OCI distribution reference grammar: a host (name,
	// IPv4 or bracketed IPv6, optional port), one or more lowercase path
	// components, an optional tag, and a sha256 digest. The path and the
	// digest are required: every reference is registry-qualified.
	refPattern = regexp.MustCompile(`^(?:[A-Za-z0-9]+(?:[-.][A-Za-z0-9]+)*|\[[0-9A-Fa-f:.]+\])(?::[0-9]+)?` +
		`(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}$`)
	tokenPattern    = regexp.MustCompile(`^[\w.-]+$`)
	versionPattern  = regexp.MustCompile(`^[\w.+-]+$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(?:/[a-z0-9]+)?$`)
)

// Release is the part of the description the install reads.
type Release struct {
	SchemaVersion         string            `json:"schemaVersion"`
	Reference             string            `json:"reference"`
	RootDigest            string            `json:"rootDigest"`
	DeclaredQualification string            `json:"declaredQualification"`
	InspectionDetails     InspectionDetails `json:"inspectionDetails"`
	SelectedOutput        string            `json:"selectedOutput"`
	TotalOutputs          int               `json:"totalOutputs"`
	Metadata              Metadata          `json:"metadata"`
	Outputs               []Artifact        `json:"outputs"`
}

// InspectionDetails says how much of the publication the description covers.
type InspectionDetails struct {
	OutputSelection string `json:"outputSelection"`
}

// Metadata records the identities the release was produced from.
type Metadata struct {
	ContentSubject string `json:"contentSubject"`
}

// Artifact is one published output.
type Artifact struct {
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Digest       string      `json:"digest"`
	Reference    string      `json:"reference"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	ChartVersion string      `json:"chartVersion"`
	ImageIndex   *ImageIndex `json:"imageIndex"`
}

// ImageIndex describes a multi-arch image: the platforms it was required to
// cover and the manifest published for each.
type ImageIndex struct {
	RequiredPlatforms []string          `json:"requiredPlatforms"`
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

// Platform returns a native output's platform as os/arch, or "" when the
// output names none: a multi-arch index, or a kind without one.
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

// Platforms returns the platforms the output is published for: the native
// platform, or the children of its image index. Empty means the output
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

// Load reads and validates the file at path. Errors name the path.
func Load(path string) (*Release, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Parse decodes and validates a description.
func Parse(data []byte) (*Release, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("the file is empty")
	}
	var r Release
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("not a JSON document: %w", err)
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Release) validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion is %q, expected %s; nvkectl reads the file `nvkr release export --reference <release>` writes, not the output of `nvkr release verify`", r.SchemaVersion, SchemaVersion)
	}
	if r.Reference == "" {
		return errors.New("reference is empty")
	}
	if !refPattern.MatchString(r.Reference) {
		return fmt.Errorf("reference is not an OCI reference with a sha256 digest: %s", r.Reference)
	}
	if !digestPattern.MatchString(r.RootDigest) {
		return fmt.Errorf("rootDigest is not sha256:<64 hex>: %q", r.RootDigest)
	}
	if !strings.HasSuffix(r.Reference, "@"+r.RootDigest) {
		return fmt.Errorf("reference %s does not end in rootDigest %s", r.Reference, r.RootDigest)
	}
	// A browse of one output writes the same schema with the rest left out;
	// rendering against it would take the missing charts from the deploy path
	// without a word.
	if sel := r.InspectionDetails.OutputSelection; (sel != "" && sel != "all") || r.SelectedOutput != "" {
		return fmt.Errorf("the description is filtered to %q; export the whole release with nvkr release export", firstNonEmpty(r.SelectedOutput, sel))
	}
	if r.TotalOutputs != 0 && r.TotalOutputs != len(r.Outputs) {
		return fmt.Errorf("totalOutputs is %d but %d outputs are listed; export the whole release", r.TotalOutputs, len(r.Outputs))
	}
	if len(r.Outputs) == 0 {
		return errors.New("outputs is empty")
	}
	names := make(map[string]bool, len(r.Outputs))
	for i, a := range r.Outputs {
		if err := a.validate(i); err != nil {
			return err
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate output name: %s", a.Name)
		}
		names[a.Name] = true
	}
	return nil
}

func (a Artifact) validate(i int) error {
	if a.Name == "" || a.Kind == "" || a.Digest == "" {
		return fmt.Errorf("outputs[%d]: name, kind and digest are required", i)
	}
	if !tokenPattern.MatchString(a.Name) {
		return fmt.Errorf("output name must be a token: %q", a.Name)
	}
	if !tokenPattern.MatchString(a.Kind) {
		return fmt.Errorf("output %s: kind must be a token: %q", a.Name, a.Kind)
	}
	if a.ChartVersion != "" && !versionPattern.MatchString(a.ChartVersion) {
		return fmt.Errorf("output %s: chartVersion is not a version: %q", a.Name, a.ChartVersion)
	}
	if !digestPattern.MatchString(a.Digest) {
		return fmt.Errorf("output %s: digest is not sha256:<64 hex>: %s", a.Name, a.Digest)
	}
	if a.Reference != "" {
		if !refPattern.MatchString(a.Reference) {
			return fmt.Errorf("output %s: reference is not an OCI reference with a sha256 digest: %s", a.Name, a.Reference)
		}
		if !strings.HasSuffix(a.Reference, "@"+a.Digest) {
			return fmt.Errorf("output %s: reference %s does not end in its digest %s", a.Name, a.Reference, a.Digest)
		}
	}
	if a.OS != "" && a.Arch == "" {
		return fmt.Errorf("output %s: os %q without arch", a.Name, a.OS)
	}
	if a.OS != "" && strings.Contains(a.Arch, "/") && !strings.HasPrefix(a.Arch, a.OS+"/") {
		return fmt.Errorf("output %s: os %q disagrees with arch %q", a.Name, a.OS, a.Arch)
	}
	if a.Arch != "" && a.ImageIndex != nil {
		return fmt.Errorf("output %s: both arch and imageIndex are set; a native output has one platform, an index lists its children", a.Name)
	}
	if a.ImageIndex != nil {
		if len(a.ImageIndex.Children) == 0 {
			return fmt.Errorf("output %s: imageIndex has no children", a.Name)
		}
		for _, c := range a.ImageIndex.Children {
			if !digestPattern.MatchString(c.ManifestDigest) {
				return fmt.Errorf("output %s: image index child %q: manifestDigest is not sha256:<64 hex>: %q", a.Name, c.Platform, c.ManifestDigest)
			}
		}
		for _, p := range a.ImageIndex.RequiredPlatforms {
			if !coversPlatform(a.Platforms(), p) {
				return fmt.Errorf("output %s: imageIndex requires platform %s but lists no child for it", a.Name, p)
			}
		}
	}
	for _, p := range a.Platforms() {
		if !platformPattern.MatchString(p) {
			return fmt.Errorf("output %s: platform %q is not os/arch", a.Name, p)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Artifact returns the output of that exact name.
func (r *Release) Artifact(name string) (Artifact, bool) {
	for _, a := range r.Outputs {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// OfKind returns the outputs of one kind, in document order. An empty kind
// returns every output.
func (r *Release) OfKind(kind string) []Artifact {
	var out []Artifact
	for _, a := range r.Outputs {
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// ErrNotPublished reports that no output of the name exists for any
// architecture. A caller that has a fallback checks for it.
var ErrNotPublished = errors.New("not in the release")

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

// Select returns the output named name for arch (linux/amd64 or
// linux/arm64). Candidates are the output of that exact name and the ones
// named <name>-<arch suffix> for any architecture. An output that names its
// platforms, natively or through its image index, is taken only when they
// include arch; one that names none counts as built for the architecture
// its own name's suffix says, else for any. The exact name for arch wins,
// then the suffixed name for arch, then the exact name naming no platform.
// No output of the name at all is ErrNotPublished; outputs only for other
// architectures, or a candidate without a reference, are errors that say so.
func (r *Release) Select(name, arch string) (Artifact, error) {
	if !contains(Archs, arch) {
		return Artifact{}, fmt.Errorf("arch %s is not one of %s", arch, strings.Join(Archs, ", "))
	}
	var exact, suffixed, anyArch *Artifact
	var others []string
	for i := range r.Outputs {
		a := &r.Outputs[i]
		bySuffix := ""
		if a.Name != name {
			bySuffix = archSuffix(a.Name)
			if bySuffix == "" || a.Name != name+"-"+bySuffix[strings.Index(bySuffix, "/")+1:] {
				continue
			}
		}
		platforms := a.Platforms()
		if len(platforms) == 0 {
			if own := archSuffix(a.Name); own != "" {
				platforms = []string{own}
			}
		}
		switch {
		case len(platforms) == 0:
			anyArch = a
		case coversPlatform(platforms, arch) && a.Name == name:
			exact = a
		case coversPlatform(platforms, arch):
			// Two suffixed outputs may both cover arch; the one named for it wins.
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
			return Artifact{}, fmt.Errorf("%s is published only for other architectures: %s", name, strings.Join(others, ", "))
		}
		return Artifact{}, fmt.Errorf("%s: %w", name, ErrNotPublished)
	}
	if chosen.Reference == "" {
		return Artifact{}, fmt.Errorf("output %s has no reference; the release description must say where it is pulled from", chosen.Name)
	}
	return *chosen, nil
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
