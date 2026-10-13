// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvkrrelease reads the release description NVKR prints: the
// nvkr.dev/release/v4 document `nvkr release verify` writes after it has
// verified a publication, bare or inside its
// nvkr.dev/release-verification-result/v1 wrapper. The nkd.dev namespace the
// framework used before it was renamed is read too. The site controller install
// and the bootstrap take every artifact they pull from it, by reference and
// digest.
//
//	{
//	  "schemaVersion": "nvkr.dev/release/v4",
//	  "subject": "<registry>/<repo>/releases@sha256:<64 hex>",
//	  "lane": "...", "scope": "...",
//	  "artifacts": [
//	    {"name": "cwe", "kind": "oci", "digest": "sha256:<64 hex>",
//	     "reference": "<registry>/<repo>@sha256:<64 hex>", "arch": "linux/arm64", ...}
//	  ],
//	  "contentSubject": "sha256:<64 hex>", "policyDigest": "sha256:<64 hex>"
//	}
//
// The document is NVKR's, so fields this package does not read are ignored.
// What it requires: the schema version, a subject, a non-empty artifact list,
// and for every artifact a name, a kind and a sha256 digest; a reference, when
// present, must be an OCI reference ending in that digest. Names are unique.
//
// Artifact names are NVKR's output names. The deploy path binds each item to
// the artifact it uses by that name (`artifact:` in the item's release.yaml),
// so no mapping lives in code. Select resolves a name for one architecture:
// NVKR publishes some outputs per architecture, `<name>` and `<name>-arm64`,
// and some as multi-arch indexes under one name; an artifact that carries
// `arch` is taken only for that architecture.
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

// Schema versions this package reads: the NVKR release description and its
// verification-result wrapper, and the namespace they carried before the
// framework was renamed.
var (
	SchemaVersions  = []string{"nvkr.dev/release/v4", "nkd.dev/release/v3"}
	WrapperVersions = []string{"nvkr.dev/release-verification-result/v1", "nkd.dev/release-verification-result/v1"}
)

// SchemaVersion is the current release description schema.
const SchemaVersion = "nvkr.dev/release/v4"

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
	tokenPattern = regexp.MustCompile(`^[\w.-]+$`)
)

// Release is the part of the description the install reads.
type Release struct {
	SchemaVersion  string     `json:"schemaVersion"`
	Subject        string     `json:"subject"`
	Lane           string     `json:"lane"`
	Scope          string     `json:"scope"`
	Artifacts      []Artifact `json:"artifacts"`
	ContentSubject string     `json:"contentSubject"`
	PolicyDigest   string     `json:"policyDigest"`
}

// Artifact is one published output.
type Artifact struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Digest      string   `json:"digest"`
	Reference   string   `json:"reference"`
	BuildDigest string   `json:"buildDigest"`
	Components  []string `json:"components"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
}

// wrapper is the verification result around a release.
type wrapper struct {
	SchemaVersion string          `json:"schemaVersion"`
	Release       json.RawMessage `json:"release"`
}

// Repository returns the reference without its digest and without a tag.
func (a Artifact) Repository() string {
	repo, _, _ := strings.Cut(a.Reference, "@")
	if slash := strings.LastIndex(repo, "/"); strings.Contains(repo[slash+1:], ":") {
		repo = repo[:slash+1+strings.Index(repo[slash+1:], ":")]
	}
	return repo
}

// Platform returns the artifact's architecture as linux/<arch>, or "" when
// the artifact names none: a multi-arch index, or a description NVKR wrote
// without one.
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

// Parse decodes and validates a description, bare or wrapped.
func Parse(data []byte) (*Release, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("the file is empty")
	}
	var head wrapper
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("not a JSON document: %w", err)
	}
	if contains(WrapperVersions, head.SchemaVersion) {
		if len(head.Release) == 0 {
			return nil, errors.New("the verification result carries no release")
		}
		data = head.Release
	}
	var r Release
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("not a release description: %w", err)
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Release) validate() error {
	if !contains(SchemaVersions, r.SchemaVersion) {
		return fmt.Errorf("schemaVersion is %q, expected %s", r.SchemaVersion, strings.Join(SchemaVersions, " or "))
	}
	if r.Subject == "" {
		return errors.New("subject is empty")
	}
	if len(r.Artifacts) == 0 {
		return errors.New("artifacts is empty")
	}
	names := make(map[string]bool, len(r.Artifacts))
	for i, a := range r.Artifacts {
		if a.Name == "" || a.Kind == "" || a.Digest == "" {
			return fmt.Errorf("artifacts[%d]: name, kind and digest are required", i)
		}
		if !tokenPattern.MatchString(a.Name) {
			return fmt.Errorf("artifact name must be a token: %q", a.Name)
		}
		if !digestPattern.MatchString(a.Digest) {
			return fmt.Errorf("artifact %s: digest is not sha256:<64 hex>: %s", a.Name, a.Digest)
		}
		if a.Reference != "" {
			if !refPattern.MatchString(a.Reference) {
				return fmt.Errorf("artifact %s: reference is not an OCI reference with a sha256 digest: %s", a.Name, a.Reference)
			}
			if !strings.HasSuffix(a.Reference, "@"+a.Digest) {
				return fmt.Errorf("artifact %s: reference %s does not end in its digest %s", a.Name, a.Reference, a.Digest)
			}
		}
		if p := a.Platform(); p != "" && !contains(Archs, p) {
			return fmt.Errorf("artifact %s: arch %s is not one of %s", a.Name, p, strings.Join(Archs, ", "))
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate artifact name: %s", a.Name)
		}
		names[a.Name] = true
	}
	return nil
}

// Artifact returns the artifact of that exact name.
func (r *Release) Artifact(name string) (Artifact, bool) {
	for _, a := range r.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// OfKind returns the artifacts of one kind, in document order. An empty kind
// returns every artifact.
func (r *Release) OfKind(kind string) []Artifact {
	var out []Artifact
	for _, a := range r.Artifacts {
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// ErrNotPublished reports that no artifact of the name exists for any
// architecture. A caller that has a fallback checks for it.
var ErrNotPublished = errors.New("not in the release")

// Select returns the artifact named name for arch (linux/amd64 or
// linux/arm64). Candidates are the artifact of that exact name and the ones
// named <name>-<arch suffix> for any architecture. An artifact that carries
// arch is taken only when it matches; a suffixed name without arch counts as
// built for the suffix. The exact name with a matching arch wins, then the
// suffixed name for arch, then the exact name naming no arch, which is how a
// multi-arch index is published. No artifact of the name at all is
// ErrNotPublished; artifacts only for other architectures, or a candidate
// without a reference, are errors that say so.
func (r *Release) Select(name, arch string) (Artifact, error) {
	if !contains(Archs, arch) {
		return Artifact{}, fmt.Errorf("arch %s is not one of %s", arch, strings.Join(Archs, ", "))
	}
	var exactArch, suffixed, noArch *Artifact
	var others []string
	for i := range r.Artifacts {
		a := &r.Artifacts[i]
		bySuffix := ""
		for _, candidate := range Archs {
			if a.Name == name+"-"+candidate[strings.Index(candidate, "/")+1:] {
				bySuffix = candidate
			}
		}
		if a.Name != name && bySuffix == "" {
			continue
		}
		p := a.Platform()
		if p == "" {
			p = bySuffix
		}
		switch {
		case p == arch && a.Name == name:
			exactArch = a
		case p == arch:
			suffixed = a
		case p == "":
			noArch = a
		default:
			others = append(others, a.Name+" ("+p+")")
		}
	}
	chosen := exactArch
	if chosen == nil {
		chosen = suffixed
	}
	if chosen == nil {
		chosen = noArch
	}
	if chosen == nil {
		if len(others) > 0 {
			return Artifact{}, fmt.Errorf("%s is published only for other architectures: %s", name, strings.Join(others, ", "))
		}
		return Artifact{}, fmt.Errorf("%s: %w", name, ErrNotPublished)
	}
	if chosen.Reference == "" {
		return Artifact{}, fmt.Errorf("artifact %s has no reference; the release description must say where it is pulled from", chosen.Name)
	}
	return *chosen, nil
}

func contains(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}
