// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package plan

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/dsx-ai-factory/nke/release/internal/policy"
)

// noDependencies names the single combination of a component that depends on
// nothing. There is exactly one, and it has no versions to be named after,
// but every combination needs an id because that is what `run` is given.
const noDependencies = "base"

// Planner resolves what to qualify. It owns no state between calls.
type Planner struct {
	// Policy is the release-wide qualification policy.
	Policy *policy.Policy

	// Root is the repository root, where component directories are found.
	Root string

	// Tags is where component versions come from.
	Tags Tags

	// History is the central artifact, consulted for the `latest` selector's
	// second tier and for what became of each cell.
	History History
}

// Plan resolves one component at one version over one scope.
//
// An empty version means the component's own latest release candidate, which
// is what a nightly wants: the thing just built is the thing to qualify.
func (p *Planner) Plan(component, version, scope string) (*Plan, error) {
	if !slices.Contains(p.Policy.Spec.Components, component) {
		return nil, fmt.Errorf(
			"%q is not a component in the qualification policy, which lists %s",
			component, strings.Join(p.Policy.Spec.Components, ", "),
		)
	}

	selector, ok := p.Policy.Spec.Scopes[scope]
	if !ok {
		return nil, fmt.Errorf("%q is not a scope in the qualification policy, which defines %s",
			scope, strings.Join(scopeNames(p.Policy), ", "))
	}

	spec, err := policy.LoadComponent(policy.ComponentPath(p.Root, component))
	if err != nil {
		return nil, err
	}
	if spec.Spec.Component != component {
		return nil, fmt.Errorf("%s/qualification.yaml describes %q, not %q",
			component, spec.Spec.Component, component)
	}

	streams, err := p.streams(selector, spec)
	if err != nil {
		return nil, err
	}

	if version == "" {
		if version, err = p.latestCandidate(component); err != nil {
			return nil, err
		}
	} else if version, err = p.checkCandidate(component, version); err != nil {
		return nil, err
	}

	combinations, err := p.combinations(spec, component, version, streams)
	if err != nil {
		return nil, err
	}

	return &Plan{
		Component:    component,
		Version:      version,
		Scope:        scope,
		Combinations: combinations,
	}, nil
}

// streams expands the scope's selector over the policy's stream list, then
// narrows to the component's own subset if it names one. Policy declaration
// order is preserved, which is what makes two runs of the same plan produce
// the same file.
func (p *Planner) streams(scope policy.Scope, spec *policy.Component) ([]policy.Stream, error) {
	var out []policy.Stream
	for _, s := range p.Policy.Spec.Streams {
		if scope.Matches(s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the scope selects no stream, so there is nowhere to run")
	}

	if len(spec.Spec.Streams) == 0 {
		return out, nil
	}

	// A name the policy has never heard of is a typo, not a narrowing, and
	// intersecting would swallow it as "selects nothing".
	for _, want := range spec.Spec.Streams {
		if !slices.ContainsFunc(p.Policy.Spec.Streams, func(s policy.Stream) bool {
			return s.Name == want
		}) {
			return nil, fmt.Errorf("%s names stream %q, which the policy does not define",
				spec.Spec.Component, want)
		}
	}

	narrowed := slices.DeleteFunc(out, func(s policy.Stream) bool {
		return !slices.Contains(spec.Spec.Streams, s.Name)
	})
	if len(narrowed) == 0 {
		return nil, fmt.Errorf(
			"%s narrows to streams the scope does not select, so there is nowhere to run",
			spec.Spec.Component,
		)
	}
	return narrowed, nil
}

// checkCandidate holds an explicitly named version to the same standard as a
// resolved dependency: it must be a version, and it must exist.
//
// Without this the thing being qualified was the one input nothing checked.
// Dependencies are resolved from tags and fail when nothing satisfies them,
// but --version was taken on trust, so a typo produced a plan naming a
// candidate that does not exist -- and every later verb would go looking for
// artifacts under it. That is the failure this stage is supposed to catch
// before anything is scheduled.
func (p *Planner) checkCandidate(component, version string) (string, error) {
	v := normalise(version)
	if !semver.IsValid(v) {
		return "", fmt.Errorf(
			"%q is not a version: expected vX.Y.Z or vX.Y.Z-rc.N, and the dot in -rc.N is required",
			version,
		)
	}

	known, err := p.versions(component)
	if err != nil {
		return "", err
	}
	if slices.Contains(known, v) {
		return v, nil
	}

	// Name what does exist. The usual cause is reaching for a candidate that
	// was never cut, and the answer is nearly always in the first few.
	const show = 5
	nearest := known
	suffix := ""
	if len(nearest) > show {
		nearest, suffix = nearest[:show], ", ..."
	}
	if len(known) == 0 {
		return "", fmt.Errorf("%s has no tagged versions, so %s cannot be one of them", component, v)
	}
	return "", fmt.Errorf("%s has no version %s; the newest are %s%s",
		component, v, strings.Join(nearest, ", "), suffix)
}

// latestCandidate is what an absent --version means: the highest release
// candidate of the component itself. Not the highest version -- a release
// would already have been qualified on its way to becoming one.
func (p *Planner) latestCandidate(component string) (string, error) {
	versions, err := p.versions(component)
	if err != nil {
		return "", err
	}
	for _, v := range versions {
		if !isRelease(v) {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s has no release candidate to qualify, so --version is required", component)
}

// versions lists a component's tags, normalised, valid, and newest first.
func (p *Planner) versions(component string) ([]string, error) {
	raw, err := p.Tags.Versions(component)
	if err != nil {
		return nil, err
	}

	var valid []string
	for _, v := range raw {
		// A tag the release workflow would reject is not a version this can
		// resolve to. Skipping rather than failing keeps one malformed tag
		// from making every plan unbuildable.
		if n := normalise(v); semver.IsValid(n) {
			valid = append(valid, n)
		}
	}
	return sortDescending(valid), nil
}

// combinations resolves every dependency and takes the product.
func (p *Planner) combinations(
	spec *policy.Component, component, version string, streams []policy.Stream,
) ([]Combination, error) {
	deps := spec.Spec.Dependencies
	if len(deps) == 0 {
		return []Combination{
			p.combination(noDependencies, nil, component, version, streams),
		}, nil
	}

	// One list of resolved versions per dependency, in declaration order.
	resolved := make([][]string, len(deps))
	for i, d := range deps {
		versions, err := p.resolve(d)
		if err != nil {
			return nil, fmt.Errorf("resolving %s for %s: %w", d.Name, component, err)
		}
		resolved[i] = versions
	}

	// The product, last dependency varying fastest. With today's single
	// `latest` selector every list has one entry and this yields exactly one
	// combination, which is the intended starting behaviour rather than a
	// special case worth branching on.
	var out []Combination
	idx := make([]int, len(deps))
	for {
		picked := make([]Dependency, len(deps))
		for i, d := range deps {
			picked[i] = Dependency{Name: d.Name, Version: resolved[i][idx[i]]}
		}
		out = append(out, p.combination(combinationID(picked), picked, component, version, streams))

		i := len(deps) - 1
		for ; i >= 0; i-- {
			idx[i]++
			if idx[i] < len(resolved[i]) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			break
		}
	}
	return out, nil
}

// combination pairs one dependency set with every stream in the scope,
// marking the cells an earlier run already settled.
func (p *Planner) combination(
	id string, deps []Dependency, component, version string, streams []policy.Stream,
) Combination {
	cells := make([]Stream, 0, len(streams))
	for _, s := range streams {
		cell := Stream{Name: s.Name, Capabilities: s.Capabilities}
		// A history that cannot answer is not a reason to refuse to plan:
		// the worst case is rerunning a cell that had already settled, which
		// costs time and proves the same thing.
		if outcome, err := p.History.Outcome(component, version, id, s.Name); err == nil && outcome.skippable() {
			cell.Skip = outcome
		}
		cells = append(cells, cell)
	}
	return Combination{ID: id, Dependencies: deps, Streams: cells}
}

// resolve turns one dependency's selectors into the versions to qualify
// against, in selector order and without repeats.
func (p *Planner) resolve(d policy.Dependency) ([]string, error) {
	versions, err := p.versions(d.Name)
	if err != nil {
		return nil, err
	}

	versions, err = matchConstraint(versions, d.VersionConstraint)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, s := range d.Selectors {
		if s != SelectorLatest {
			return nil, fmt.Errorf("selector %q is not one this understands; the only selector is %q",
				s, SelectorLatest)
		}
		v, err := selectLatest(versions, func(v string) (bool, error) {
			return p.History.Qualified(d.Name, v)
		})
		if err != nil {
			if d.VersionConstraint == "" {
				return nil, fmt.Errorf("%s has no versions at all", d.Name)
			}
			return nil, fmt.Errorf("no version of %s satisfies %q", d.Name, d.VersionConstraint)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// combinationID names a combination by what varies in it, so the id in a plan
// and the id in a published record are the same string without either side
// having to look the other up.
func combinationID(deps []Dependency) string {
	parts := make([]string, len(deps))
	for i, d := range deps {
		parts[i] = d.Name + "-" + d.Version
	}
	return strings.Join(parts, "_")
}

func scopeNames(p *policy.Policy) []string {
	names := make([]string, 0, len(p.Spec.Scopes))
	for name := range p.Spec.Scopes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
