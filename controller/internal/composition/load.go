// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	registryFile = "registry.yaml"

	apiVersion      = "nke.nvidia.com/v1alpha1"
	registryKind    = "ComponentRegistry"
	compositionKind = "Composition"
)

// Recipe is a composition resolved against its registry, with every component
// reference checked. Load returns nothing partially valid: a reference to an
// unknown component, a dependency edge to a component the composition does not
// select, or a missing values file is an error here rather than a chart that
// renders and then fails to install.
type Recipe struct {
	Registry    Registry
	Composition Composition
	Dir         string

	// byName indexes the registry for lookups during emission.
	byName map[string]Component
}

// Load reads registry.yaml and the named composition from dir.
func Load(dir, compositionFile string) (*Recipe, error) {
	r := &Recipe{Dir: dir, byName: map[string]Component{}}

	if err := readYAML(filepath.Join(dir, registryFile), &r.Registry); err != nil {
		return nil, err
	}
	if err := checkHeader(registryFile, r.Registry.APIVersion, r.Registry.Kind, registryKind); err != nil {
		return nil, err
	}
	for _, c := range r.Registry.Components {
		if _, dup := r.byName[c.Name]; dup {
			return nil, fmt.Errorf("%s: component %q declared twice", registryFile, c.Name)
		}
		r.byName[c.Name] = c
	}

	if err := readYAML(filepath.Join(dir, compositionFile), &r.Composition); err != nil {
		return nil, err
	}
	if err := checkHeader(compositionFile, r.Composition.APIVersion, r.Composition.Kind, compositionKind); err != nil {
		return nil, err
	}
	if err := r.validate(compositionFile); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Recipe) validate(file string) error {
	spec := r.Composition.Spec
	if spec.ReleaseNamespace == "" {
		return fmt.Errorf("%s: spec.releaseNamespace is required", file)
	}

	selected := map[string]bool{}
	for _, ref := range spec.ComponentRefs {
		if _, known := r.byName[ref.Name]; !known {
			return fmt.Errorf("%s: component %q is not in %s", file, ref.Name, registryFile)
		}
		if selected[ref.Name] {
			return fmt.Errorf("%s: component %q selected twice", file, ref.Name)
		}
		if r.byName[ref.Name].Helm.DefaultChart == "" {
			// EmitChart resolves the dependency by this name. Empty emits a
			// nameless dependency that helm rejects only at install time.
			return fmt.Errorf("%s: component %q has no helm.defaultChart", file, ref.Name)
		}
		selected[ref.Name] = true
	}

	for _, ref := range spec.ComponentRefs {
		for _, dep := range ref.DependsOn {
			if !selected[dep] {
				// A dangling edge would silently drop an ordering constraint,
				// which is how a race gets shipped.
				return fmt.Errorf("%s: %q dependsOn %q, which this composition does not select",
					file, ref.Name, dep)
			}
		}
		if ref.ValuesFile == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(r.Dir, ref.ValuesFile)); err != nil {
			return fmt.Errorf("%s: %q valuesFile: %w", file, ref.Name, err)
		}
	}

	for _, req := range spec.Requires {
		if req.Before != "" && !selected[req.Before] {
			return fmt.Errorf("%s: requires %q is declared before %q, which this composition does not select",
				file, req.Name, req.Before)
		}
	}
	return nil
}

// Component returns the registry entry for a selected component.
func (r *Recipe) Component(name string) Component { return r.byName[name] }

func readYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	// Reject a key the structs do not model rather than ignoring it: a typo in a
	// recipe field would otherwise be silently dropped.
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func checkHeader(file, gotAPI, gotKind, wantKind string) error {
	if gotAPI != apiVersion {
		return fmt.Errorf("%s: apiVersion %q, want %q", file, gotAPI, apiVersion)
	}
	if gotKind != wantKind {
		return fmt.Errorf("%s: kind %q, want %q", file, gotKind, wantKind)
	}
	return nil
}
