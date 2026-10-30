// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package stackinstall resolves the NVKE controller stack for a seed or a
// site: the Helm releases with their values and overlays, and the glue
// manifests, from the embedded stack directory.
package stackinstall

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed stack/values stack/manifests
var embedded embed.FS

// stackFS is the embedded stack directory, so paths read values/... and
// manifests/....
var stackFS = func() fs.FS {
	sub, err := fs.Sub(embedded, "stack")
	if err != nil {
		panic(err)
	}
	return sub
}()

// chart is one Helm release: its name, the artifact output that holds the
// chart, the values/<values>.yaml it takes, and which targets install it.
type chart struct {
	release string
	output  string
	values  string
	seed    bool
	site    bool
	// nico rows are planned only when the artifact file has the nico output.
	nico bool
}

// charts is the release table. The order of the install is not here.
var charts = []chart{
	{release: "nvke-controller", output: "controller-chart", values: "controller", seed: true, site: true},
	{release: "nktp", output: "nktp", values: "nktp", site: true},
	{release: "postgres", output: "postgresql", values: "postgresql", seed: true, site: true},
	{release: "spire-crds", output: "spire-crds", seed: true, site: true},
	{release: "spire", output: "spire", values: "spire", seed: true, site: true},
	{release: "temporal", output: "temporal", values: "temporal", seed: true, site: true},
	{release: "nico", output: "nico", values: "nico", seed: true, nico: true},
	// values/nico-prereqs.yaml is written for the Bitnami postgresql chart.
	{release: "nico-prereqs", output: "postgresql", values: "nico-prereqs", seed: true, nico: true},
}

func (c chart) planned(kind TargetKind, hasNico bool) bool {
	if c.nico && !hasNico {
		return false
	}
	return kind == Seed && c.seed || kind == Site && c.site
}

const (
	nicoManifests = "manifests/nico/"
	nktpManifest  = "manifests/nktp-cluster.yaml.tmpl"
)

func manifestPlanned(path string, kind TargetKind, hasNico bool) bool {
	switch {
	case strings.HasPrefix(path, nicoManifests):
		return kind == Seed && hasNico
	case path == nktpManifest:
		return kind == Site
	default:
		return true
	}
}
