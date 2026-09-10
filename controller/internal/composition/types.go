// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package composition turns the recipe under controller/deploy/composition into
// the nke-stack umbrella chart.
//
// The recipe is two files. registry.yaml says what each component IS -- chart
// coordinates, a version, and the namespace its resources land in -- once,
// independent of any composition. nke-stack.yaml says which components ship
// together, how they depend on each other, and what the deployment needs that
// the chart does not own.
//
// How a chart is relocated (namespaceOverride vs namespace, fullnameOverride,
// per-subchart keys) is a property of the upstream chart rather than of the
// composition, so it lives in that component's values file and is copied through
// verbatim. That also keeps it re-expressible by a consumer that nests this
// chart and never runs this generator.
package composition

// Registry is the component catalog: controller/deploy/composition/registry.yaml.
type Registry struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Components []Component `yaml:"components"`
}

// Component is one entry in the registry.
type Component struct {
	Name        string `yaml:"name"`
	DisplayName string `yaml:"displayName"`
	Helm        Helm   `yaml:"helm"`

	// Upstream is set for a chart that is not published anywhere and has to be
	// built from source -- spire, pinned to a commit for jwtIssuer support that
	// has no upstream release.
	Upstream *Upstream `yaml:"upstream,omitempty"`

	// Source is set for a chart that lives in this repository.
	Source *Source `yaml:"source,omitempty"`
}

type Helm struct {
	DefaultRepository string `yaml:"defaultRepository"`
	DefaultChart      string `yaml:"defaultChart"`
	// DerivedVersion is the sentinel for a first-party chart whose version comes
	// from `git describe`: the pin does not exist until the tag does.
	DefaultVersion   string `yaml:"defaultVersion"`
	DefaultNamespace string `yaml:"defaultNamespace,omitempty"`
}

type Upstream struct {
	Repo   string `yaml:"repo"`
	Commit string `yaml:"commit"`
	Path   string `yaml:"path"`
}

type Source struct {
	Path string `yaml:"path"`
}

// DerivedVersion marks a version supplied at build time rather than authored.
const (
	DerivedVersion = "derived"
)

// Composition is one deployable set: controller/deploy/composition/nke-stack.yaml.
type Composition struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type Spec struct {
	// ReleaseNamespace is pinned to the one component that cannot be relocated:
	// temporal's core templates follow .Release.Namespace with no override.
	ReleaseNamespace string `yaml:"releaseNamespace"`

	// Namespaces records who creates them. This chart never does; see the recipe.
	Namespaces map[string]string `yaml:"namespaces"`

	// Globals are release-wide -- Helm merges `global` into every subchart at
	// every depth -- so they belong to the composition, not to a component.
	Globals map[string]any `yaml:"globals"`

	ComponentRefs []ComponentRef `yaml:"componentRefs"`

	// Requires are preconditions the composition needs and deliberately does not
	// own. helm --wait only waits on workloads, so these are checked explicitly.
	Requires []Requirement `yaml:"requires"`
}

// ComponentRef selects a registry component into this composition.
type ComponentRef struct {
	Name string `yaml:"name"`
	// ValuesFile is relative to the recipe directory. Absent means the component
	// is enabled with no configuration.
	ValuesFile string `yaml:"valuesFile,omitempty"`
	// DependsOn are edges, not phases: install order is a topological sort of
	// this graph, so the phase list is derived rather than asserted.
	DependsOn []string `yaml:"dependsOn,omitempty"`
}

type Requirement struct {
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
	Namespace  string `yaml:"namespace"`
	Condition  string `yaml:"condition"`
	ProvidedBy string `yaml:"providedBy,omitempty"`
	Before     string `yaml:"before,omitempty"`
}

// Phase is a group of components with no dependency between them, installed
// together and waited on before the next phase begins.
type Phase struct {
	Name       string   `yaml:"name"`
	Components []string `yaml:"components"`
}
