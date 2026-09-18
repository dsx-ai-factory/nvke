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

// SBOMSet is delivery rather than composition: which artifact sets are cut from
// the chart and for whom. controller/deploy/composition/nke-sbom.yaml. Optional
// -- a consumer that wants only the chart needs no such file.
type SBOMSet struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   Metadata    `yaml:"metadata"`
	Spec       SBOMSetSpec `yaml:"spec"`
}

type SBOMSetSpec struct {
	SBOMs []SBOM `yaml:"sboms"`
}

// SBOM is one generated artifact set: a directory of deploy paths, one per
// target, published as a unit or not at all.
type SBOM struct {
	Name string `yaml:"name"`
	// Publish decides whether the push target will touch this tree. It is a
	// property of the set rather than a command-line flag: the e2e trees are
	// built by lanes that deliberately hold no registry credentials, so they
	// must not be pushable by typo.
	Publish bool `yaml:"publish"`

	// Registry is the OCI repository an emitted Application resolves the chart
	// from -- the repoURL of its helm source, in OCI-Helm form, which carries no
	// scheme and no chart name. Required for a set with an argocd target;
	// unused by one without, because a helm target is handed a chart by
	// whoever runs the install.
	Registry string `yaml:"registry,omitempty"`

	Targets []Target `yaml:"targets"`
}

// Target is one deploy path: a provider, an overlay, and how the umbrella
// arrives on the cluster.
type Target struct {
	Name string `yaml:"name"`
	// Provider names what the capi phase installs. `provisioned` adopts a host
	// that already exists rather than creating one, which is what a bootstrap
	// cluster needs: the machine being provisioned is the one running kind.
	Provider string `yaml:"provider"`
	// Overlay is relative to the recipe directory, and is copied into the
	// target so the generated tree is self-contained -- a published set is
	// tarred and pushed, so it cannot reference files outside itself.
	Overlay string `yaml:"overlay"`
	// Stack is how the umbrella arrives: StackHelm installs it directly, one
	// release per phase, and is the only option where no ArgoCD exists;
	// StackArgoCD emits Applications for a site that outlives its installation.
	Stack string `yaml:"stack"`

	// BaseConfig names the files from base-config/ this target carries, without
	// their numeric prefix or extension. Namespaces are always generated; these
	// are not inferred, because which credentials a cluster holds should be read
	// off the target rather than derived from a rule that can change under it.
	//
	// oidc-upstream-ca is resolved from the composition's own requires block
	// instead of base-config/, so the chart keeps one source of truth for the
	// precondition it declares.
	BaseConfig []string `yaml:"baseConfig,omitempty"`

	// Manifests names the pins under manifests/ this target installs, in the
	// order they are applied, between base-config and the umbrella. Each is a
	// vendored upstream manifest with its edits baked in at generate time, so
	// the item is a plain apply.
	Manifests []string `yaml:"manifests,omitempty"`
}

// The values Target.Stack accepts.
const (
	StackHelm   = "helm"
	StackArgoCD = "argocd"
)

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
