// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import "fmt"

// Policy is release/policy.yaml: the matrix a release can be qualified
// against and how much of it a run covers.
type Policy struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   Metadata   `json:"metadata,omitempty"`
	Spec       PolicySpec `json:"spec"`
}

// PolicySpec carries the streams and the named selections over them.
type PolicySpec struct {
	Scopes  map[string]Scope   `json:"scopes"`
	Streams []StreamDefinition `json:"streams"`
}

// Scope is a named selection over the streams, matched on capabilities. A
// scope with no selector matches everything, which is how `full` is spelled.
type Scope struct {
	Selector map[string]string `json:"selector,omitempty"`
}

// StreamDefinition is one stream as the policy declares it: a configuration a
// subject can be qualified against, named by the capabilities it provides
// rather than by the infrastructure that satisfies them. A Stream in a plan is
// what resolved for one of these.
type StreamDefinition struct {
	Name         string            `json:"name"`
	Capabilities map[string]string `json:"capabilities"`
}

// Matches reports whether a stream satisfies this scope's selector. Every
// selector key must be present on the stream and equal, and a scope with no
// selector matches every stream.
func (s Scope) Matches(stream StreamDefinition) bool {
	for k, want := range s.Selector {
		if stream.Capabilities[k] != want {
			return false
		}
	}
	return true
}

// LoadPolicy reads and validates release/policy.yaml.
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

func (p *Policy) validate(path string) error {
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
