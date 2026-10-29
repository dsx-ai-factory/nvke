// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifacts reads the artifact file, the JSON document that names
// every image and chart nvkectl installs. Release is the contract: its
// fields are decoded and every other key is dropped, so an unmodified
// release export from nvkr (github.com/dsx-ai-factory/nvkr) and a
// hand-written file with only outputs both satisfy it. Output names are the
// ones in nvkr's combined NVKE definition
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

// Output kinds as nvkr writes them.
const (
	KindImage = "oci"
	KindChart = "helm-oci"
)

// Expectation is one output nvkectl reads: its name, the kind it must have
// and whether the file must carry it.
type Expectation struct {
	Name     string
	Kind     string
	Required bool
}

// Expected lists every output nvkectl reads, in the order validation reports
// them. nico-prereqs is required when nico is present. local-path-provisioner
// is required when the target cluster has no default StorageClass.
var Expected = []Expectation{
	{Name: "cwe", Kind: KindImage, Required: true},
	{Name: "capnico-controller", Kind: KindImage, Required: true},
	{Name: "controller-chart", Kind: KindChart, Required: true},
	{Name: "nktp", Kind: KindChart, Required: true},
	{Name: "postgresql", Kind: KindChart, Required: true},
	{Name: "spire-crds", Kind: KindChart, Required: true},
	{Name: "spire", Kind: KindChart, Required: true},
	{Name: "temporal", Kind: KindChart, Required: true},
	{Name: "nico", Kind: KindChart},
	{Name: "nico-prereqs", Kind: KindChart},
	{Name: "local-path-provisioner", Kind: KindChart},
}

// Release is the decoded artifact file.
type Release struct {
	// RootDigest identifies the release the file describes. It is recorded
	// in the install record, not verified against the content.
	RootDigest string   `json:"rootDigest"`
	Outputs    []Output `json:"outputs"`
}

// Output is one image or chart. Reference is the digest-pinned OCI
// reference nvkectl pulls.
type Output struct {
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

// Decode reads one JSON object and rejects anything after it. It does not
// validate the content.
func Decode(r io.Reader) (*Release, error) {
	var rel *Release
	dec := json.NewDecoder(r)
	if err := dec.Decode(&rel); err != nil {
		return nil, fmt.Errorf("artifact file is not valid JSON: %w", err)
	}
	if rel == nil {
		return nil, errors.New("artifact file is not a JSON object")
	}
	_, err := dec.Token()
	var syntax *json.SyntaxError
	switch {
	case errors.Is(err, io.EOF):
		return rel, nil
	case err == nil, errors.As(err, &syntax):
		return nil, errors.New("artifact file has content after the JSON object")
	default:
		return nil, fmt.Errorf("artifact file: %w", err)
	}
}

// Output returns the output of that exact name, or nil.
func (rel *Release) Output(name string) *Output {
	for i := range rel.Outputs {
		if rel.Outputs[i].Name == name {
			return &rel.Outputs[i]
		}
	}
	return nil
}

// Platforms returns the os/arch pairs an image serves, as nvkr writes them:
// the image index children when there are any, else OS and Arch. A chart
// has none.
func (o *Output) Platforms() []string {
	if o.ImageIndex != nil && len(o.ImageIndex.Children) > 0 {
		platforms := make([]string, 0, len(o.ImageIndex.Children))
		for _, c := range o.ImageIndex.Children {
			platforms = append(platforms, c.Platform)
		}
		return platforms
	}
	if o.OS != "" && o.Arch != "" {
		return []string{o.OS + "/" + o.Arch}
	}
	return nil
}
