// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"regexp"
)

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
// Exclude takes back the streams that carry any one of its capabilities, so a
// scope can cover a provider without covering a stream that only shares it.
type Scope struct {
	Selector map[string]string `json:"selector,omitempty"`
	Exclude  map[string]string `json:"exclude,omitempty"`
}

// StreamDefinition is one stream as the policy declares it: a configuration a
// subject can be qualified against, named by the capabilities it provides
// rather than by the infrastructure that satisfies them. A Stream in a plan is
// what resolved for one of these.
type StreamDefinition struct {
	Name         string            `json:"name"`
	Capabilities map[string]string `json:"capabilities"`

	// Parameters configure how the stream runs.
	Parameters Parameters `json:"parameters,omitzero"`
}

// Parameters are the settings of a stream's run. Capabilities say what a
// stream is, and a scope selects on them and the suite matches machines by
// them. Nothing is matched against a parameter: it only configures the run, so
// it is declared here and never selected on.
//
// The fields are the ones a run can be given, and a policy that names another
// is rejected, so a misspelled parameter is an error and not a setting that
// quietly does nothing. A field left unset leaves the run as it is for a
// stream that names no parameters.
type Parameters struct {
	// Environment names the e2e environment file the stream runs against, in
	// place of the one its provider maps to.
	Environment string `json:"environment,omitempty"`

	// TenantWorkers is how many workers the hardware tenant gets.
	TenantWorkers int `json:"tenantWorkers,omitempty"`

	// SuiteTimeoutMinutes is how long the suite may run before Ginkgo stops it
	// and tears the machines down. The timeouts around it are derived from
	// this one, so they cannot be set to disagree.
	SuiteTimeoutMinutes int `json:"suiteTimeoutMinutes,omitempty"`
}

// environmentName is what an environment file's name may be: the name of a
// file beside the others, never a path.
var environmentName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validate rejects an environment that is not a file name and a count that is
// negative.
func (p Parameters) validate() error {
	if p.Environment != "" && !environmentName.MatchString(p.Environment) {
		return fmt.Errorf("environment %q is not an environment file's name", p.Environment)
	}
	if p.TenantWorkers < 0 {
		return fmt.Errorf("tenantWorkers is %d, and a tenant needs at least one worker", p.TenantWorkers)
	}
	if p.SuiteTimeoutMinutes < 0 {
		return fmt.Errorf("suiteTimeoutMinutes is %d, which is not a duration", p.SuiteTimeoutMinutes)
	}
	return nil
}

// Matches reports whether a stream satisfies this scope. Every selector key
// must be present on the stream and equal, and a stream carrying any exclude
// capability is left out. A scope with no selector matches every stream it
// does not exclude.
func (s Scope) Matches(stream StreamDefinition) bool {
	for k, want := range s.Selector {
		if stream.Capabilities[k] != want {
			return false
		}
	}
	for k, unwanted := range s.Exclude {
		if stream.Capabilities[k] == unwanted {
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

// validate checks that the policy can be used: it has streams and scopes, no
// two streams share a name, every selector and exclude names a capability some
// stream declares, and each stream's parameters are valid.
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
		if err := s.Parameters.validate(); err != nil {
			return fmt.Errorf("%s: stream %q: %w", path, s.Name, err)
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
		for k, v := range scope.Exclude {
			if !known[k] {
				return fmt.Errorf(
					"%s: scope %q excludes capability %q, which no stream declares",
					path, name, k,
				)
			}
			if v == "" {
				return fmt.Errorf("%s: scope %q excludes capability %q with no value", path, name, k)
			}
		}
	}
	return nil
}
