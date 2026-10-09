// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"strings"
)

// Registry is release/registry.yaml: how each component resolves to a version.
// It holds no versions, only the rule that finds them.
type Registry struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata,omitempty"`
	Spec       RegistrySpec `json:"spec"`
}

// RegistrySpec lists every component that can be resolved, and the named
// locations they resolve against.
type RegistrySpec struct {
	// Sources are named locations, such as the CDN artifacts are published
	// under. A component names one with source in its resolve.with, so a
	// location is stated once.
	Sources []Source `json:"sources,omitempty"`

	Components []Component `json:"components"`
}

// Source is a named location. It holds no version, only where something is.
type Source struct {
	Name string `json:"name"`
	URL  string `json:"url"`
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

// StrategyConfig returns the flat configuration a component's strategy takes,
// with a source expanded into the base it names. The strategies only ever see a
// base, which keeps them free of this package's types, and the one place that
// turns a source into one is here.
func (r *Registry) StrategyConfig(c Component) map[string]string {
	if c.Resolve.With == nil {
		return nil
	}

	with := make(map[string]string, len(c.Resolve.With))
	for k, v := range c.Resolve.With {
		with[k] = v
	}
	if name, ok := with["source"]; ok {
		delete(with, "source")
		for _, src := range r.Spec.Sources {
			if src.Name == name {
				with["base"] = src.URL
			}
		}
	}
	return with
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
	sources := map[string]bool{}
	for _, src := range r.Spec.Sources {
		switch {
		case src.Name == "":
			return fmt.Errorf("%s: a source has no name", path)
		case sources[src.Name]:
			return fmt.Errorf("%s: source %q is listed twice", path, src.Name)
		case !strings.HasPrefix(src.URL, "https://") && !strings.HasPrefix(src.URL, "http://"):
			return fmt.Errorf("%s: source %q has url %q, which is not a URL", path, src.Name, src.URL)
		}
		sources[src.Name] = true
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
		if err := checkSource(path, c, sources); err != nil {
			return err
		}
		seen[c.Name] = true
	}
	return nil
}

// checkSource holds a component's source to the registry's sources. A source
// that is not listed would expand to nothing, and a source beside a base would
// leave two answers to where it looks.
func checkSource(path string, c Component, sources map[string]bool) error {
	name, ok := c.Resolve.With["source"]
	if !ok {
		return nil
	}
	if !sources[name] {
		return fmt.Errorf("%s: component %q names source %q, which is not listed", path, c.Name, name)
	}
	if _, ok := c.Resolve.With["base"]; ok {
		return fmt.Errorf("%s: component %q names both a source and a base", path, c.Name)
	}
	return nil
}
