// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package policy reads the qualification.yaml files: the release-wide policy
// and one spec per component.
//
// It is a separate package from plan because plan is not its only reader. A
// run publishes its result using the ComponentQualification kind with the
// selectors resolved and a status attached, so publish needs these types too;
// putting them in plan would make the recorder import the planner to borrow a
// struct.
package policy

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// APIVersion is the group and version both kinds carry. There is one, and a
// file naming another is rejected rather than coerced: the alternative is
// reading a future schema with today's structs and silently dropping the
// fields that were the reason for the bump.
const APIVersion = "nke.nvidia.com/v1alpha1"

// Kinds.
const (
	KindQualificationPolicy    = "QualificationPolicy"
	KindComponentQualification = "ComponentQualification"
)

// Policy is release/qualification.yaml: what must be qualified across the
// whole release.
type Policy struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       Spec   `json:"spec"`
}

// Spec carries the components in scope, the streams they run against,
// and the named selections over those streams.
type Spec struct {
	Components []string         `json:"components"`
	Scopes     map[string]Scope `json:"scopes"`
	Streams    []Stream         `json:"streams"`
}

// Scope is a named selection over the streams, matched on capabilities. A
// scope with no selector matches everything, which is how `full` is spelled.
type Scope struct {
	Selector map[string]string `json:"selector,omitempty"`
}

// Stream is one configuration a component can be qualified against, named by
// the capabilities it provides rather than by the infrastructure that
// satisfies them.
type Stream struct {
	Name         string            `json:"name"`
	Capabilities map[string]string `json:"capabilities"`
}

// Component is <component>/qualification.yaml: how one component is
// qualified.
type Component struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   Metadata      `json:"metadata"`
	Spec       ComponentSpec `json:"spec"`
}

// Metadata names the component spec and says what it is for.
type Metadata struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ComponentSpec is the body of a ComponentQualification.
type ComponentSpec struct {
	Component    string       `json:"component"`
	Dependencies []Dependency `json:"dependencies,omitempty"`

	// Streams narrows this component to a subset of the scope's streams.
	// Empty means every stream the scope selects, which is the common case.
	Streams []string `json:"streams,omitempty"`
}

// Dependency is one other component this one is qualified against, and how to
// pick its version.
type Dependency struct {
	Name string `json:"name"`

	// VersionConstraint excludes versions before resolution. Empty means no
	// floor, which is honest while nothing has shipped a release.
	VersionConstraint string `json:"versionConstraint,omitempty"`

	// Selectors is a list so that qualifying against the last N qualified
	// versions of a dependency stays a widening of what a selector expresses
	// rather than a redesign. Today the only selector is `latest`.
	Selectors []string `json:"selectors"`
}

// Matches reports whether a stream satisfies this scope's selector. Every
// selector key must be present on the stream and equal; a scope with no
// selector matches every stream.
func (s Scope) Matches(stream Stream) bool {
	for k, want := range s.Selector {
		if stream.Capabilities[k] != want {
			return false
		}
	}
	return true
}

// LoadPolicy reads and validates release/qualification.yaml.
func LoadPolicy(path string) (*Policy, error) {
	var p Policy
	if err := readStrict(path, &p); err != nil {
		return nil, err
	}
	if err := checkTypeMeta(path, p.APIVersion, p.Kind, KindQualificationPolicy); err != nil {
		return nil, err
	}
	if err := p.validate(path); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadComponent reads and validates <dir>/qualification.yaml, checking
// that the file describes the component whose directory it sits in.
func LoadComponent(path string) (*Component, error) {
	var c Component
	if err := readStrict(path, &c); err != nil {
		return nil, err
	}
	if err := checkTypeMeta(path, c.APIVersion, c.Kind, KindComponentQualification); err != nil {
		return nil, err
	}
	if err := c.validate(path); err != nil {
		return nil, err
	}
	return &c, nil
}

// ComponentPath is where a component's spec lives, derived from its name.
// spec.component maps to the component directory, which is also its tag
// prefix, so one string is the directory, the tag prefix and the identity.
func ComponentPath(root, component string) string {
	return filepath.Join(root, component, "qualification.yaml")
}

// readStrict unmarshals YAML and rejects fields the structs do not declare. A
// misspelled key would otherwise read as an omitted one, and every field here
// is optional-looking enough that the result would be a plan quietly missing
// whatever the typo was meant to say.
func readStrict(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(b, into); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

func checkTypeMeta(path, apiVersion, kind, wantKind string) error {
	if apiVersion != APIVersion {
		return fmt.Errorf("%s: apiVersion is %q, want %q", path, apiVersion, APIVersion)
	}
	if kind != wantKind {
		return fmt.Errorf("%s: kind is %q, want %q", path, kind, wantKind)
	}
	return nil
}

func (p *Policy) validate(path string) error {
	if len(p.Spec.Components) == 0 {
		return fmt.Errorf("%s: spec.components is empty, so nothing can be qualified", path)
	}
	if len(p.Spec.Streams) == 0 {
		return fmt.Errorf("%s: spec.streams is empty, so no plan could name anywhere to run", path)
	}
	if len(p.Spec.Scopes) == 0 {
		return fmt.Errorf("%s: spec.scopes is empty, so no run could say how much of the matrix it covers", path)
	}

	// Capability keys any stream declares. A selector on a key no stream has
	// matches nothing, and an empty scope is indistinguishable from a scope
	// whose capacity is merely busy, so the typo is caught here instead.
	known := map[string]bool{}
	seen := map[string]bool{}
	for _, s := range p.Spec.Streams {
		if s.Name == "" {
			return fmt.Errorf("%s: a stream has no name", path)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s: two streams are named %q", path, s.Name)
		}
		seen[s.Name] = true
		if len(s.Capabilities) == 0 {
			return fmt.Errorf("%s: stream %q declares no capabilities, so no selector can reach it", path, s.Name)
		}
		for k := range s.Capabilities {
			known[k] = true
		}
	}

	for name, scope := range p.Spec.Scopes {
		for k := range scope.Selector {
			if !known[k] {
				return fmt.Errorf(
					"%s: scope %q selects on capability %q, which no stream declares",
					path, name, k,
				)
			}
		}
	}
	return nil
}

func (c *Component) validate(path string) error {
	if c.Spec.Component == "" {
		return fmt.Errorf("%s: spec.component is empty", path)
	}
	if c.Metadata.Name != "" && c.Metadata.Name != c.Spec.Component {
		return fmt.Errorf("%s: metadata.name is %q but spec.component is %q",
			path, c.Metadata.Name, c.Spec.Component)
	}

	seen := map[string]bool{}
	for _, d := range c.Spec.Dependencies {
		switch {
		case d.Name == "":
			return fmt.Errorf("%s: a dependency has no name", path)
		case d.Name == c.Spec.Component:
			return fmt.Errorf("%s: %q depends on itself", path, d.Name)
		case seen[d.Name]:
			return fmt.Errorf("%s: %q is listed twice", path, d.Name)
		case len(d.Selectors) == 0:
			return fmt.Errorf("%s: dependency %q has no selectors, so it resolves to nothing", path, d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}
