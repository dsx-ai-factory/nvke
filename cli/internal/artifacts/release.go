// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifacts reads the artifact file, the JSON document that names
// every image and chart nvkectl installs. Release is the contract: its
// fields are decoded and every other key is dropped, so an unmodified
// release export from nvkr (github.com/dsx-ai-factory/nvkr) and a
// hand-written file with only an artifacts list both satisfy it. nvkr names
// that list outputs; Decode reads either key. Artifact names are the output
// names in nvkr's combined NVKE definition
// (inputs/definitions/nvke/definition.yaml). nico and nico-prereqs are in the
// core family (inputs/definitions/nvke/core.yaml) only, and
// local-path-provisioner is in neither.
//
// cli/internal/bootstrap/nvkrrelease reads the same document for the deploy
// path.
package artifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Artifact kinds as nvkr writes them.
const (
	KindImage = "oci"
	KindChart = "helm-oci"
)

// Expectation is one artifact nvkectl reads: its name, the kind it must have,
// whether the file must carry it and, for a chart, the versions the embedded
// values target.
type Expectation struct {
	Name     string
	Kind     string
	Required bool
	// RequiredWith is the name of another artifact. When the file has that
	// artifact, it must have this one too.
	RequiredWith  string
	ChartVersions VersionRange
}

// Expected lists every artifact nvkectl reads, in the order validation reports
// them. local-path-provisioner is required when the target cluster has no
// default StorageClass; preflight checks that, not Validate.
var Expected = []Expectation{
	{Name: "cwe", Kind: KindImage, Required: true},
	{Name: "capnico-controller", Kind: KindImage, Required: true},
	{Name: "controller-chart", Kind: KindChart, Required: true, ChartVersions: VersionRange{Version{0, 22, 0}, Version{0, 24, 0}}},
	{Name: "nktp", Kind: KindChart, Required: true, ChartVersions: VersionRange{Version{0, 22, 0}, Version{0, 24, 0}}},
	{Name: "postgresql", Kind: KindChart, Required: true, ChartVersions: VersionRange{Version{18, 5, 15}, Version{19, 0, 0}}},
	{Name: "spire-crds", Kind: KindChart, Required: true, ChartVersions: VersionRange{Version{0, 6, 1}, Version{0, 7, 0}}},
	{Name: "spire", Kind: KindChart, Required: true, ChartVersions: VersionRange{Version{0, 30, 1}, Version{0, 31, 0}}},
	{Name: "temporal", Kind: KindChart, Required: true, ChartVersions: VersionRange{Version{0, 74, 0}, Version{0, 75, 0}}},
	{Name: "nico", Kind: KindChart, ChartVersions: VersionRange{Version{2, 2, 2}, Version{3, 0, 0}}},
	// values/nico-prereqs.yaml targets the Bitnami postgresql chart.
	{Name: "nico-prereqs", Kind: KindChart, RequiredWith: "nico", ChartVersions: VersionRange{Version{18, 5, 15}, Version{19, 0, 0}}},
	{Name: "local-path-provisioner", Kind: KindChart, ChartVersions: VersionRange{Version{0, 0, 31}, Version{0, 1, 0}}},
}

// Release is the decoded artifact file.
type Release struct {
	// RootDigest identifies the release the file describes. It is recorded
	// in the install record, not verified against the content.
	RootDigest string     `json:"rootDigest"`
	Artifacts  []Artifact `json:"artifacts"`
}

// releaseFile is the JSON shape Decode reads. outputs is nvkr's name for
// the artifacts list.
type releaseFile struct {
	RootDigest string      `json:"rootDigest"`
	Artifacts  *[]Artifact `json:"artifacts"`
	Outputs    *[]Artifact `json:"outputs"`
}

// Artifact is one image or chart. Reference is the digest-pinned OCI
// reference nvkectl pulls.
type Artifact struct {
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Reference    string      `json:"reference"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	ImageIndex   *ImageIndex `json:"imageIndex"`
	ChartVersion string      `json:"chartVersion"`
}

// ImageIndex lists the per-platform manifests of a multi-platform image.
type ImageIndex struct {
	Children []ImageIndexChild `json:"children"`
}

// ImageIndexChild is one manifest of an image index.
type ImageIndexChild struct {
	Platform string `json:"platform"`
}

// Decode reads one JSON object and rejects anything after it. The list is
// read from artifacts or outputs; a file with both is rejected. Validate
// checks the content.
func Decode(r io.Reader) (*Release, error) {
	var f *releaseFile
	dec := json.NewDecoder(r)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("artifact file is not valid JSON: %w", err)
	}
	if f == nil {
		return nil, errors.New("artifact file is not a JSON object")
	}
	_, err := dec.Token()
	var syntax *json.SyntaxError
	switch {
	case errors.Is(err, io.EOF):
	case err == nil, errors.As(err, &syntax):
		return nil, errors.New("artifact file has content after the JSON object")
	default:
		return nil, fmt.Errorf("artifact file: %w", err)
	}
	rel := &Release{RootDigest: f.RootDigest}
	switch {
	case f.Artifacts != nil && f.Outputs != nil:
		return nil, errors.New(`artifact file has both "artifacts" and "outputs"`)
	case f.Artifacts != nil:
		rel.Artifacts = *f.Artifacts
	case f.Outputs != nil:
		rel.Artifacts = *f.Outputs
	}
	return rel, nil
}

// Artifact returns the artifact of that exact name, or nil.
func (rel *Release) Artifact(name string) *Artifact {
	for i := range rel.Artifacts {
		if rel.Artifacts[i].Name == name {
			return &rel.Artifacts[i]
		}
	}
	return nil
}

// Platforms returns the os/arch pairs an image serves, as nvkr writes them:
// the image index children when there are any, else OS and Arch. A chart
// has none.
func (a *Artifact) Platforms() []string {
	if a.ImageIndex != nil && len(a.ImageIndex.Children) > 0 {
		platforms := make([]string, 0, len(a.ImageIndex.Children))
		for _, c := range a.ImageIndex.Children {
			platforms = append(platforms, c.Platform)
		}
		return platforms
	}
	if a.OS != "" && a.Arch != "" {
		return []string{a.OS + "/" + a.Arch}
	}
	return nil
}
