// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import "fmt"

// Registry is release/registry.yaml: how each component resolves to a version.
// It holds no versions, only the rule that finds them.
type Registry struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata,omitempty"`
	Spec       RegistrySpec `json:"spec"`
}

// RegistrySpec lists every component that can be resolved.
type RegistrySpec struct {
	Components []Component `json:"components"`
}

// Component is one resolvable thing and the strategy that resolves it. A
// component's name is also its key in the suite's -e2e.versions flag.
type Component struct {
	Name    string  `json:"name"`
	Resolve Resolve `json:"resolve"`
}

// Resolve names a strategy and the flat configuration it takes. The
// configuration is a map of strings so that the resolve package stays free of
// the types in this one.
type Resolve struct {
	Strategy string            `json:"strategy"`
	With     map[string]string `json:"with,omitempty"`
}

// Component returns the registry entry with this name.
func (r *Registry) Component(name string) (Component, bool) {
	for _, c := range r.Spec.Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// LoadRegistry reads and validates release/registry.yaml.
func LoadRegistry(path string) (*Registry, error) {
	var r Registry
	if err := readStrict(path, &r); err != nil {
		return nil, err
	}
	if err := checkTypeMeta(path, r.APIVersion, r.Kind, KindRegistry); err != nil {
		return nil, err
	}
	if err := r.validate(path); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Registry) validate(path string) error {
	if len(r.Spec.Components) == 0 {
		return fmt.Errorf("%s: spec.components is empty, so nothing can be resolved", path)
	}
	seen := map[string]bool{}
	for _, c := range r.Spec.Components {
		switch {
		case c.Name == "":
			return fmt.Errorf("%s: a component has no name", path)
		case seen[c.Name]:
			return fmt.Errorf("%s: %q is listed twice", path, c.Name)
		case c.Resolve.Strategy == "":
			return fmt.Errorf("%s: component %q names no strategy", path, c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}
