// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/release/internal/artifact"
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

// Source is a named location that components are published under. It holds no
// version, only where things are.
//
// It is a base and never a whole location: a component names the source and
// supplies the rest, a path under it for the CDN and a repository and version
// for a registry. StrategyConfig hands it to a strategy as "base", which is
// what the strategies call it.
type Source struct {
	Name string `json:"name"`
	Base string `json:"base"`
}

// Component is one resolvable thing and how it resolves. A component's name is
// also its key in the suite's -e2e.versions flag.
type Component struct {
	Name    string  `json:"name"`
	Resolve Resolve `json:"resolve"`
}

// Resolve answers two questions about an entry, which are separate questions
// with separate answers.
//
// What version is this, and where is that version published. Most entries
// answer only the first. One that is published somewhere this can name answers
// both, and a bootloader answers only the second, because a declared path has
// no version behind it.
type Resolve struct {
	// Version says what version this is, by naming a strategy or by taking
	// another entry's. Absent for an entry that is only a location.
	Version *Rule `json:"version,omitempty"`

	// Location says where that version is. Absent for an entry that publishes
	// nowhere this names.
	Location *Rule `json:"location,omitempty"`
}

// Rule is one half of a resolution: a strategy and the flat configuration it
// takes, or, for a version, the entry to take it from.
//
// The configuration is a map of strings so that the resolve package stays free
// of the types in this one. From is meaningless for a location, which is
// rejected rather than ignored.
type Rule struct {
	Strategy string            `json:"strategy,omitempty"`
	With     map[string]string `json:"with,omitempty"`
	From     string            `json:"from,omitempty"`
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

// StrategyConfig returns the flat configuration one half's strategy takes, with
// a source expanded into the base it names. The strategies only ever see a
// base, which keeps them free of this package's types, and the one place that
// turns a source into one is here.
func (r *Registry) StrategyConfig(rule *Rule) map[string]string {
	if rule == nil || rule.With == nil {
		return nil
	}

	with := make(map[string]string, len(rule.With))
	for k, v := range rule.With {
		with[k] = v
	}
	if name, ok := with["source"]; ok {
		delete(with, "source")
		for _, src := range r.Spec.Sources {
			if src.Name == name {
				with["base"] = src.Base
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
		case !artifact.IsScheme(src.Base):
			return fmt.Errorf("%s: source %q has base %q, which names no scheme a location can be "+
				"checked under", path, src.Name, src.Base)
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
		}
		if err := checkRules(path, c); err != nil {
			return err
		}
		for _, rule := range []*Rule{c.Resolve.Version, c.Resolve.Location} {
			if err := checkSource(path, c.Name, rule, sources); err != nil {
				return err
			}
		}
		seen[c.Name] = true
	}
	return nil
}

// checkRules holds an entry's two halves to what each can be. An entry with
// neither resolves nothing, and a half that is half-specified would resolve by
// a rule nobody wrote.
func checkRules(path string, c Component) error {
	version, location := c.Resolve.Version, c.Resolve.Location
	switch {
	case version == nil && location == nil:
		return fmt.Errorf("%s: component %q resolves neither a version nor a location, so it "+
			"answers nothing", path, c.Name)
	case version != nil && version.Strategy != "" && version.From != "":
		return fmt.Errorf("%s: component %q names a version strategy and a version to take from "+
			"%q, which are two answers to one question", path, c.Name, version.From)
	case version != nil && version.Strategy == "" && version.From == "":
		return fmt.Errorf("%s: component %q has a version that names neither a strategy nor a "+
			"component to take one from", path, c.Name)
	case location != nil && location.From != "":
		// A location is where a version is, and the version half already says
		// whose version that is.
		return fmt.Errorf("%s: component %q has a location with from %q, which a location does "+
			"not take", path, c.Name, location.From)
	case location != nil && location.Strategy == "":
		return fmt.Errorf("%s: component %q has a location that names no strategy", path, c.Name)
	}
	return nil
}

// checkSource holds a half's source to the registry's sources. A source that is
// not listed would expand to nothing, and a source beside a base would leave
// two answers to where it looks.
func checkSource(path, name string, rule *Rule, sources map[string]bool) error {
	if rule == nil {
		return nil
	}
	src, ok := rule.With["source"]
	if !ok {
		return nil
	}
	if !sources[src] {
		return fmt.Errorf("%s: component %q names source %q, which is not listed", path, name, src)
	}
	if _, ok := rule.With["base"]; ok {
		return fmt.Errorf("%s: component %q names both a source and a base", path, name)
	}
	return nil
}
