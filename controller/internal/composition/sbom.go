// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// stackItem is the item name the umbrella is installed under, and therefore the
// directory holding its descriptor. For an argocd target it is also the
// Application name, which deploy-repository.md requires to equal the directory.
const (
	stackItem      = "nke-stack"
	baseConfigItem = "nke-base-config"

	// baseConfigDir holds the secret templates targets select from. Namespaces
	// are generated rather than authored: the registry already records where
	// every component's resources land.
	baseConfigDir = "base-config"

	// oidcCARef is the one base-config entry resolved from the composition's
	// requires block instead of base-config/, so the chart keeps one source of
	// truth for the precondition it declares.
	oidcCARef = "oidc-upstream-ca"

	// TypeTemplate and TypeManifest are two of the three item types
	// docs/site-bootstrap/deploy-repository.md defines. A base-config item is
	// the first when it carries .yaml.tmpl files and the second when it does
	// not, so a consumer with no renderer can still apply one that needs none.
	TypeTemplate = "template"
	TypeManifest = "manifest"

	// providerNico is the provider whose controller reads nico-credentials from
	// a namespace nothing else creates.
	providerNico = "capnico"
)

// EmitSBOMs renders every declared SBOM into deploy paths, keyed by path
// relative to the generated directory.
//
// The layout is the one docs/site-bootstrap/deploy-repository.md defines: a
// deploy path holds nke-deploy.yaml at its root and one subdirectory per item,
// with the item list carrying only name and type. What an item IS lives in its
// directory, which is why the chart reference is release.yaml beside the values
// rather than fields on the item -- the only change to the published contract
// is one new type value, not a new field set.
//
// Only the umbrella item is emitted today. The base-config, cert-manager and
// capi items need pinned upstream manifests that this repository does not carry
// yet; they arrive with the pins, and the item list grows without the shape
// changing.
func (r *Recipe) EmitSBOMs() (map[string][]byte, error) {
	files := map[string][]byte{}

	seen := map[string]string{}
	for _, sbom := range r.SBOMs.Spec.SBOMs {
		if sbom.Name == "" {
			return nil, fmt.Errorf("sboms: an entry has no name")
		}
		for _, target := range sbom.Targets {
			// An Application naming a chart with no repoURL cannot sync, and
			// nothing downstream supplies one, so the failure would arrive on
			// the cluster rather than here.
			if target.Stack == StackArgoCD && sbom.Registry == "" {
				return nil, fmt.Errorf("sboms: %s has an argocd target (%s) and no registry, so its Applications would carry no repoURL",
					sbom.Name, target.Name)
			}
			if prev, dup := seen[target.Name]; dup {
				return nil, fmt.Errorf("sboms: target %q declared in both %s and %s", target.Name, prev, sbom.Name)
			}
			seen[target.Name] = sbom.Name

			targetFiles, err := r.emitTarget(sbom, target)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", sbom.Name, target.Name, err)
			}
			for path, content := range targetFiles {
				files[filepath.Join(sbom.Name, target.Name, path)] = content
			}
		}
	}
	return files, nil
}

// emitTarget renders one deploy path.
func (r *Recipe) emitTarget(sbom SBOM, target Target) (map[string][]byte, error) {
	if target.Overlay == "" {
		return nil, fmt.Errorf("no overlay")
	}
	// Copied rather than referenced: a published set is tarred and pushed, so a
	// deploy path that reaches outside itself is one an operator cannot use.
	overlay, err := os.ReadFile(filepath.Join(r.Dir, target.Overlay))
	if err != nil {
		return nil, fmt.Errorf("reading overlay: %w", err)
	}

	files := map[string][]byte{}
	var items []item

	// base-config is always first: every later item names something it creates.
	// CAPNICo reads nico-credentials at startup and its manifest names the pull
	// secret, and the umbrella's identity phase blocks on the OIDC CA.
	base, baseType, err := r.emitBaseConfig(target)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", baseConfigItem, err)
	}
	items = append(items, item{Name: baseConfigItem, Type: baseType})
	for path, content := range base {
		files[filepath.Join(baseConfigItem, path)] = content
	}

	// Vendored manifests come between base-config and the umbrella: CAPNICo
	// reads the credentials the first item creates, and the storage class has to
	// exist before anything asks for a volume.
	manifests, manifestItems, err := r.emitManifestItems(target)
	if err != nil {
		return nil, err
	}
	items = append(items, manifestItems...)
	for path, content := range manifests {
		files[path] = content
	}

	switch target.Stack {
	case StackHelm:
		items = append(items, item{Name: stackItem, Type: StackHelm})
		files[filepath.Join(stackItem, "release.yaml")] = r.emitRelease(target)
		files[filepath.Join(stackItem, "values.yaml")] = overlay

	case StackArgoCD:
		phases, err := r.Phases()
		if err != nil {
			return nil, err
		}
		// One item per phase, each in its own directory: the published contract
		// requires an argocd item's metadata.name to equal its directory name,
		// so three Applications cannot share one.
		for i, phase := range phases {
			name := stackItem + "-" + phase.Name
			items = append(items, item{Name: name, Type: StackArgoCD})
			files[filepath.Join(name, "application.yaml")] = r.emitApplication(sbom, target, phases, i, overlay)
		}

	default:
		return nil, fmt.Errorf("stack: want %q or %q, got %q", StackHelm, StackArgoCD, target.Stack)
	}

	deploy, err := r.emitDeployList(sbom, target, items)
	if err != nil {
		return nil, err
	}
	files["nke-deploy.yaml"] = deploy
	return files, nil
}

// item is one entry of nke-deploy.yaml. The published schema defines name, type
// and an optional path, and nothing else: what an item IS lives in its
// directory.
type item struct {
	Name string
	Type string
}

// emitDeployList writes nke-deploy.yaml: items in install order, carrying only
// what the published schema defines.
func (r *Recipe) emitDeployList(sbom SBOM, target Target, items []item) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, `# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by %s from controller/deploy/composition. Do not edit.
#
# Deploy path for target %q of %q (provider %s, published: %t).
#
# Items are applied in the order below -- the list IS the order, which is what
# docs/site-bootstrap/deploy-repository.md specifies. An item carries only its
# name and type; what it is lives in its directory.
items:
`, generatedBy, target.Name, sbom.Name, target.Provider, sbom.Publish)
	for _, it := range items {
		fmt.Fprintf(&b, "  - {name: %s, type: %s}\n", it.Name, it.Type)
	}

	var check map[string]any
	if err := yaml.Unmarshal(b.Bytes(), &check); err != nil {
		return nil, fmt.Errorf("generated nke-deploy.yaml is not valid YAML: %w", err)
	}
	return b.Bytes(), nil
}

// emitRelease writes release.yaml: which chart, which version, and how the
// umbrella is installed.
//
// perPhase is FALSE on this path. A helm target installs the umbrella as one
// release, upgraded once per phase from the chart's own phases: list -- the
// loop cwedeploy already runs. One release per phase is the ARGOCD shape, where
// each phase is its own Application and Helm is not doing the sequencing.
//
// What the phase loop must not do is assert the positive. Emitting
// `--set <dep>.enabled=true` for everything up to the phase is what let the old
// build/nke-stack.sh install override an overlay -- --set beats -f, so
// capd-e2e's disabled kube-prometheus-stack came back on. Naming only the
// components still ahead of the phase, and saying nothing about the rest,
// sequences the install without touching what the overlay decided.
func (r *Recipe) emitRelease(target Target) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by %s from controller/deploy/composition. Do not edit.
#
# The chart reference for target %q. Where the chart is resolved FROM is a
# parameter of the install, not of this file: the lanes pass a locally packaged
# .tgz and need no registry, while a published set names an OCI repository.
chart:
  name: %s
  # 0.0.0 as committed, stamped at publish, matching the umbrella itself.
  version: 0.0.0
release:
  prefix: %s
  namespace: %s
  perPhase: false
values:
  - values.yaml
`, generatedBy, target.Name, stackItem, stackItem, r.Composition.Spec.ReleaseNamespace)
	return b.Bytes()
}

// emitApplication writes the ArgoCD Application for one phase of the umbrella.
//
// Every phase's Application carries the identical chart and revision; only the
// slice differs, and it is expressed as the COMPLEMENT -- the components this
// phase does not install. Asserting the positive would override an overlay that
// disabled one of them.
//
// The complement rides in helm.parameters, which ArgoCD passes as --set, rather
// than as extra keys appended to the values block. Two top-level keys of the
// same name in one values document are a duplicate key, not a later-wins
// override: a mapping REPLACES rather than merges, so appending
// `controller: {enabled: false}` after an overlay's configured `controller:`
// block would discard the block -- supportedInfraProviders, the CAPI RBAC switch
// and dgxcTenantCredSecretName with it. A --set names one leaf and leaves its
// siblings alone, which is the semantics this needs, and it keeps the inlined
// overlay byte-identical to the file it came from.
//
// The overlay is inlined rather than referenced. A helm source's valueFiles
// resolve inside the CHART, not inside this deploy path, so naming values.yaml
// there would silently deploy chart defaults; reaching a file outside the chart
// needs a second source Argo can fetch, which a published artifact has no
// address for. Inlining makes regenerating the Application the way to change
// values, which is what the pinned revision means anyway.
func (r *Recipe) emitApplication(sbom SBOM, target Target, phases []Phase, index int, overlay []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by %s from controller/deploy/composition. Do not edit.
#
# Phase %d of %d for target %q. The phases are disjoint, so the releases own
# non-overlapping resources; Release.Namespace is the same for all of them,
# because a component with no namespaceOverride falls through to it.
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s-%s
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "%d"
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  destination:
    server: https://kubernetes.default.svc
    namespace: %s
  source:
    # OCI-Helm form, which carries no scheme. ArgoCD needs a repository
    # credential covering this URL with type helm and enableOCI set; that
    # credential is the provisioning script's job, like every other secret.
    repoURL: %s
    chart: %s
    targetRevision: "0.0.0"
    helm:
      values: |
`, generatedBy, index+1, len(phases), target.Name, stackItem, phases[index].Name, index+1,
		r.Composition.Spec.ReleaseNamespace, sbom.Registry, stackItem)

	for _, line := range strings.Split(strings.TrimRight(string(overlay), "\n"), "\n") {
		if line == "" {
			fmt.Fprintln(&b)
			continue
		}
		fmt.Fprintf(&b, "        %s\n", line)
	}

	if after := componentsAfter(phases, index); len(after) > 0 {
		fmt.Fprint(&b, "      parameters:\n")
		for _, name := range after {
			fmt.Fprintf(&b, "        - {name: %s.enabled, value: \"false\"}\n", name)
		}
	}

	fmt.Fprint(&b, `  syncPolicy:
    automated: {prune: true, selfHeal: true}
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
`)
	return b.Bytes()
}

// componentsAfter lists the components of every phase other than index, which is
// the set a phase's Application must disable.
//
// Every other phase, not just the later ones: a release owns exactly its own
// slice. Disabling only what comes after leaves each release cumulative, so the
// second renders the first's components too -- three releases in one namespace
// claiming overlapping resources, and with prune and selfHeal set on every
// Application, claiming them continuously.
func componentsAfter(phases []Phase, index int) []string {
	var out []string
	for i, phase := range phases {
		if i == index {
			continue
		}
		out = append(out, phase.Components...)
	}
	return out
}

// emitBaseConfig renders the first item of a deploy path, and reports whether
// it is a template or a manifest item.
//
// Namespaces are generated from the registry rather than authored: helm
// --create-namespace makes only the release namespace, and the composition
// deliberately emits no Namespace objects of its own, so something has to. The
// PodSecurity labels travel with them because spire-agent and spiffe-csi-driver
// need hostNetwork, hostPID and privileged containers, which `restricted`
// rejects -- and kind does not enforce Pod Security, so a missing label surfaces
// only on a cluster that does.
//
// Everything else is selected by name on the target. Which credentials a
// cluster holds is read off the recipe rather than inferred, so a rule change
// cannot silently add or drop one.
func (r *Recipe) emitBaseConfig(target Target) (map[string][]byte, string, error) {
	namespaces, err := r.namespaces(target)
	if err != nil {
		return nil, "", err
	}
	files := map[string][]byte{"00-namespaces.yaml": namespaces}

	itemType := TypeManifest
	for _, name := range target.BaseConfig {
		path, content, err := r.baseConfigFile(name)
		if err != nil {
			return nil, "", err
		}
		if strings.HasSuffix(path, ".tmpl") {
			itemType = TypeTemplate
		}
		files[path] = content
	}
	return files, itemType, nil
}

// baseConfigFile resolves one entry of a target's baseConfig list.
func (r *Recipe) baseConfigFile(name string) (string, []byte, error) {
	if name == oidcCARef {
		for _, req := range r.Composition.Spec.Requires {
			if req.ProvidedBy == "" {
				continue
			}
			// Matched by filename rather than by position: a second requires
			// entry carrying a providedBy would otherwise be emitted here under
			// this name, and the mismatch would surface on a cluster.
			base := filepath.Base(req.ProvidedBy)
			if strings.TrimSuffix(base, filepath.Ext(base)) != name {
				continue
			}
			// providedBy is repository-relative; the recipe sits three levels in.
			content, err := os.ReadFile(filepath.Join(r.Dir, "..", "..", "..", req.ProvidedBy))
			if err != nil {
				return "", nil, fmt.Errorf("reading %s: %w", req.ProvidedBy, err)
			}
			return "40-" + base, content, nil
		}
		return "", nil, fmt.Errorf("%s: no requires entry is providedBy a file named %s.yaml", oidcCARef, oidcCARef)
	}

	entries, err := os.ReadDir(filepath.Join(r.Dir, baseConfigDir))
	if err != nil {
		return "", nil, err
	}
	for _, entry := range entries {
		// Entries are named <NN>-<name>.yaml[.tmpl]; the target selects by name.
		base := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".tmpl"), ".yaml")
		if _, rest, found := strings.Cut(base, "-"); found && rest == name {
			content, err := os.ReadFile(filepath.Join(r.Dir, baseConfigDir, entry.Name()))
			if err != nil {
				return "", nil, err
			}
			return entry.Name(), content, nil
		}
	}
	return "", nil, fmt.Errorf("no file in %s/ named %q", baseConfigDir, name)
}

// namespaces renders every namespace the chart's components land in, plus the
// ones the deployment needs that no component declares.
func (r *Recipe) namespaces(target Target) ([]byte, error) {
	wanted := map[string]bool{
		r.Composition.Spec.ReleaseNamespace: true,
		// CWE publishes each tenant's kubeconfig here and the chart scopes its
		// Secret grant to it.
		"nke-system": true,
		// The agent lands here rather than beside the server; that split is what
		// the two PodSecurity levels below exist for. Removed below when the
		// target disables SPIRE.
		"spire-system": true,
	}
	disabled, err := r.disabledComponents(target)
	if err != nil {
		return nil, err
	}
	for _, ref := range r.Composition.Spec.ComponentRefs {
		component, known := r.byName[ref.Name]
		if !known {
			return nil, fmt.Errorf("component %q is not in %s", ref.Name, registryFile)
		}
		// A namespace for a component this target turns off would be created
		// empty and stay that way, which reads on the cluster as something that
		// failed to install rather than something never asked for.
		if disabled[ref.Name] {
			continue
		}
		if ns := component.Helm.DefaultNamespace; ns != "" {
			wanted[ns] = true
		}
	}
	// spire-agent lands here rather than beside the server, so it goes when
	// SPIRE does.
	if disabled["spire"] {
		delete(wanted, "spire-system")
	}
	// A precondition's namespace is needed only while the component it gates is
	// installed: `before` says what the requirement is for, so a target that
	// disables that component neither waits on the precondition nor needs
	// somewhere to put it.
	for _, req := range r.Composition.Spec.Requires {
		if req.Namespace == "" || disabled[req.Before] {
			continue
		}
		wanted[req.Namespace] = true
	}
	if target.Provider == providerNico {
		// CAPNICo reads nico-credentials from this exact namespace, and nothing
		// else creates it.
		wanted["capnico-system"] = true
	}

	names := make([]string, 0, len(wanted))
	for ns := range wanted {
		names = append(names, ns)
	}
	sort.Strings(names)

	var b bytes.Buffer
	fmt.Fprintf(&b, `# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by %s from controller/deploy/composition. Do not edit.
#
# Namespaces for target %q, derived from the registry's defaultNamespace fields.
# The chart emits none of its own: a consumer creates them, and
# helm --create-namespace makes only the release namespace.
`, generatedBy, target.Name)
	for _, ns := range names {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns)
		switch ns {
		case "spire-system":
			fmt.Fprint(&b, "  labels:\n    # spire-agent and spiffe-csi-driver need hostNetwork,\n    # hostPID and privileged containers.\n    pod-security.kubernetes.io/enforce: privileged\n")
		case "spire-server":
			fmt.Fprint(&b, "  labels:\n    pod-security.kubernetes.io/enforce: restricted\n")
		}
	}
	return b.Bytes(), nil
}

// disabledComponents reports which components a target's overlay turns off.
//
// The generator reads the overlay only for this: a namespace whose component is
// disabled would be created empty and stay empty, which on a cluster reads as a
// failed install rather than a deliberate omission. It deliberately does not
// interpret anything else in the overlay -- values are copied through verbatim,
// so the chart remains the only thing that decides what they mean.
func (r *Recipe) disabledComponents(target Target) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(r.Dir, target.Overlay))
	if err != nil {
		return nil, fmt.Errorf("reading overlay: %w", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s: %w", target.Overlay, err)
	}

	disabled := map[string]bool{}
	for _, ref := range r.Composition.Spec.ComponentRefs {
		section, ok := values[ref.Name].(map[string]any)
		if !ok {
			continue
		}
		// Absent means enabled: the chart's own default, which every component
		// in the composition sets to true.
		if enabled, ok := section["enabled"].(bool); ok && !enabled {
			disabled[ref.Name] = true
		}
	}
	return disabled, nil
}
