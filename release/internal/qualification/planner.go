// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// SelectorLatest is the only selector that exists. Anything else in a
// component's selectors is rejected at plan time rather than ignored, because
// ignoring it would resolve the component by a rule nobody wrote.
const SelectorLatest = "latest"

// Planner resolves what to qualify. It owns no state between calls.
type Planner struct {
	// Policy is the matrix and the scopes over it.
	Policy *Policy

	// Registry says how each component resolves.
	Registry *Registry

	// VersionResolvers and LocationResolvers hold one resolver per registry
	// component that declares that half, built by the caller. The planner never
	// names a strategy, so tests substitute fakes.
	VersionResolvers  map[string]resolve.Resolver
	LocationResolvers map[string]resolve.Resolver

	// Root is the repository root, where subject files are found.
	Root string

	// Overrides are versions a person chose for components and dependencies,
	// by name. Each replaces what the strategy would have resolved, and goes
	// through the same resolver so that it is checked there.
	Overrides map[string]string

	// ResolveAtCommit describes the components at the subject's commit rather
	// than resolving them from their own tags.
	//
	// The commit is not given, it is derived from the version being qualified,
	// so nothing can name a version and a commit that disagree. A beta tag
	// carries no component tags, so its components have to be described at the
	// commit it marks or they resolve to the last release instead.
	//
	// The commit is offered to every resolver, and the ones with no
	// commit-shaped answer ignore it, the way a resolver that does not choose
	// ignores a select. An override still wins, so a version a person named
	// means the same thing either way.
	ResolveAtCommit bool

	// commit is what ResolveAtCommit resolved to, kept for the plan to record.
	commit string
}

// Plan resolves one subject at one version over one scope.
//
// An empty version means the subject's own highest version, which resolves the
// way every other component does.
func (p *Planner) Plan(subject, version, scope string) (*Plan, error) {
	s, err := LoadSubject(SubjectPath(p.Root, subject))
	if err != nil {
		return nil, err
	}
	if s.Spec.Subject != subject {
		return nil, fmt.Errorf("%s describes %q, not %q", SubjectPath(p.Root, subject), s.Spec.Subject, subject)
	}
	if err := Check(p.Policy, p.Registry, s, p.VersionResolvers, p.LocationResolvers); err != nil {
		return nil, err
	}
	if err := checkOverrides(s, p.Overrides); err != nil {
		return nil, err
	}

	selector, ok := p.Policy.Spec.Scopes[scope]
	if !ok {
		return nil, fmt.Errorf("%q is not a scope in the qualification policy, which defines %s",
			scope, strings.Join(scopeNames(p.Policy), ", "))
	}

	streams, err := p.streams(selector, s)
	if err != nil {
		return nil, err
	}

	var ref string
	if version, ref, err = p.subjectVersion(subject, version); err != nil {
		return nil, err
	}
	if p.ResolveAtCommit {
		// From the ref the subject resolved to, whether a person named the
		// version or the tags answered, so one run has one commit and it is
		// always the one the version points at.
		if p.commit, err = resolve.CommitOf(p.Root, ref); err != nil {
			return nil, fmt.Errorf("the commit to describe %s at: %w", subject, err)
		}
	}

	combinations, err := p.combinations(s, streams)
	if err != nil {
		return nil, err
	}

	var overrides []string
	if len(p.Overrides) > 0 {
		overrides = sortedNames(p.Overrides)
	}

	return &Plan{
		Subject:      subject,
		Version:      version,
		Scope:        scope,
		Commit:       p.commit,
		Overrides:    overrides,
		Combinations: combinations,
	}, nil
}

// subjectVersion resolves the subject through the registry exactly as any
// other component resolves. An empty version is the highest version of any kind.
func (p *Planner) subjectVersion(subject, version string) (resolved, ref string, err error) {
	inputs := map[string]string{}
	if version != "" {
		inputs[resolve.InputOverrideKey] = version
	}

	got, err := p.versionResolver(subject).Resolve(inputs)
	if err != nil {
		if version == "" {
			return "", "", fmt.Errorf("resolving %s: %w", subject, err)
		}
		return "", "", fmt.Errorf("checking %s %s: %w", subject, version, err)
	}
	return got[resolve.ResultVersionKey], got[resolve.ResultRefKey], nil
}

// streams expands the scope's selector over the policy's stream list, then
// narrows to the subject's own subset if it names one. Policy declaration
// order is preserved, which is what makes two runs of the same plan produce
// the same file.
func (p *Planner) streams(scope Scope, s *Subject) ([]StreamDefinition, error) {
	var out []StreamDefinition
	for _, stream := range p.Policy.Spec.Streams {
		if scope.Matches(stream) {
			out = append(out, stream)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the scope selects no stream, so there is nowhere to run")
	}

	if len(s.Spec.Streams) == 0 {
		return out, nil
	}

	narrowed := slices.DeleteFunc(out, func(stream StreamDefinition) bool {
		return !slices.Contains(s.Spec.Streams, stream.Name)
	})
	if len(narrowed) == 0 {
		return nil, fmt.Errorf(
			"%s narrows to streams the scope does not select, so there is nowhere to run",
			s.Spec.Subject,
		)
	}
	return narrowed, nil
}

// pick is one component resolved to one version, with everything the strategy
// said about it.
type pick struct {
	name   string
	result map[string]string
}

// combinations resolves every component and takes the product.
func (p *Planner) combinations(s *Subject, streams []StreamDefinition) ([]Combination, error) {
	// One list of resolved versions per component, in declaration order.
	options := make([][]pick, len(s.Spec.Components))
	for i, c := range s.Spec.Components {
		picks, err := p.resolveComponent(c)
		if err != nil {
			return nil, fmt.Errorf("resolving %s for %s: %w", c.Name, s.Spec.Subject, err)
		}
		options[i] = picks
	}

	// The product, last component varying fastest. With today's single
	// `latest` selector every list has one entry and this yields exactly one
	// combination, which is the intended starting behaviour rather than a
	// special case worth branching on.
	var out []Combination
	idx := make([]int, len(options))
	for {
		picked := make([]pick, len(options))
		for i := range options {
			picked[i] = options[i][idx[i]]
		}

		combination, err := p.combination(s, picked, streams)
		if err != nil {
			return nil, err
		}
		out = append(out, combination)

		i := len(idx) - 1
		for ; i >= 0; i-- {
			idx[i]++
			if idx[i] < len(options[i]) {
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

// inputs are what every resolution starts from, before the component's own
// override. One place builds them so that a run cannot resolve one component at
// the commit and another from its tags.
func (p *Planner) inputs() map[string]string {
	inputs := map[string]string{}
	if p.commit != "" {
		inputs[resolve.InputCommitKey] = p.commit
	}
	return inputs
}

// resolveComponent turns one component's selectors into the versions to
// qualify, in selector order and without repeats.
func (p *Planner) resolveComponent(c SubjectComponent) ([]pick, error) {
	inputs := p.inputs()
	if v, ok := p.Overrides[c.Name]; ok {
		inputs[resolve.InputOverrideKey] = v
	}

	var out []pick
	for _, sel := range c.Selectors {
		if sel != SelectorLatest {
			return nil, fmt.Errorf("selector %q is not one this understands, the only selector is %q",
				sel, SelectorLatest)
		}
		got, err := p.resolveEntry(c.Name, maps.Clone(inputs))
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(out, func(o pick) bool { return o.result[resolve.ResultVersionKey] == got[resolve.ResultVersionKey] }) {
			out = append(out, pick{name: c.Name, result: got})
		}
	}
	return out, nil
}

// combination resolves everything around one set of components. A dependency
// bound to a stream capability is resolved once per stream and recorded under
// that stream, and every other dependency is resolved once and recorded on the
// combination.
func (p *Planner) combination(s *Subject, picked []pick, streams []StreamDefinition) (Combination, error) {
	components := make([]ComponentVersion, len(picked))

	// What a later strategy may read about the components already resolved. A
	// component's version is available under its name, and anything else its
	// strategy returned is available as "<name>.<key>".
	base := map[string]string{}
	for i, c := range picked {
		components[i] = p.componentVersion(c.name, c.result)
		base[c.name] = c.result[resolve.ResultVersionKey]
		for k, v := range c.result {
			if k != resolve.ResultVersionKey {
				base[c.name+"."+k] = v
			}
		}
	}

	common, err := p.resolveDependencies(s, base, nil, false)
	if err != nil {
		return Combination{}, err
	}

	planned := make([]Stream, 0, len(streams))
	for _, stream := range streams {
		perStream, err := p.resolveDependencies(s, base, &stream, true)
		if err != nil {
			return Combination{}, fmt.Errorf("stream %s: %w", stream.Name, err)
		}
		planned = append(planned, Stream{Name: stream.Name, Capabilities: stream.Capabilities, Dependencies: perStream})
	}

	return Combination{
		ID:           combinationID(components),
		Components:   components,
		Dependencies: common,
		Streams:      planned,
	}, nil
}

// resolveDependencies resolves the subject's dependencies that are per stream
// when perStream is set and the ones that are not when it is not.
func (p *Planner) resolveDependencies(s *Subject, base map[string]string, stream *StreamDefinition, perStream bool) ([]ComponentVersion, error) {
	var out []ComponentVersion
	for _, d := range s.Spec.Dependencies {
		if d.perStream() != perStream {
			continue
		}

		declared, err := d.inputs(stream)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name, err)
		}
		inputs := maps.Clone(base)
		maps.Copy(inputs, declared)
		if v, ok := p.Overrides[d.Name]; ok {
			inputs[resolve.InputOverrideKey] = v
		}

		got, err := p.resolveEntry(d.Name, inputs)
		if err != nil {
			return nil, fmt.Errorf("resolving %s for %s: %w", d.Name, s.Spec.Subject, err)
		}
		out = append(out, p.componentVersion(d.Name, got))
	}
	return out, nil
}

// componentVersion records what one name resolved to, and how each half
// answered.
//
// The version strategy is the one the resolver reported, so that an entry which
// can resolve two ways says which it did. One that reports nothing is only
// itself, and the registry's name for it is the honest answer. An entry that
// takes another's version names no strategy, because none answered.
//
// The location strategy is the registry's name alone. No location strategy has
// arms to choose between.
func (p *Planner) componentVersion(name string, got map[string]string) ComponentVersion {
	out := ComponentVersion{
		Name:            name,
		Version:         got[resolve.ResultVersionKey],
		URL:             got[resolve.ResultURLKey],
		VersionStrategy: got[resolve.ResultStrategyKey],
	}

	c, ok := p.Registry.Component(name)
	if !ok {
		return out
	}
	if out.VersionStrategy == "" && c.Resolve.Version != nil {
		out.VersionStrategy = c.Resolve.Version.Strategy
	}
	if c.Resolve.Location != nil {
		out.LocationStrategy = c.Resolve.Location.Strategy
	}
	return out
}

// versionResolver returns the resolver for a component's version half. A
// missing one is a wiring error and not a user error, because Check has already
// held every name to the registry.
func (p *Planner) versionResolver(name string) resolve.Resolver {
	r, ok := p.VersionResolvers[name]
	if !ok {
		return missing{name}
	}
	return r
}

// resolveEntry answers both questions about one registry entry.
//
// The version half answers first, either with a strategy of its own or by
// taking another entry's version from inputs, and the location half is then
// told which version it is naming. An entry may declare either half alone.
//
// An override names what a strategy would have produced, so it goes to the half
// that has one, and to the version half when both do. That is what makes
// --overrides controller=<version> a version and --overrides
// os-image-amd64=<url> a location without anything declaring which it is.
func (p *Planner) resolveEntry(name string, inputs map[string]string) (map[string]string, error) {
	c, ok := p.Registry.Component(name)
	if !ok {
		return nil, fmt.Errorf("%s is not a component in the registry", name)
	}

	override := inputs[resolve.InputOverrideKey]
	delete(inputs, resolve.InputOverrideKey)

	got := map[string]string{}
	switch {
	case c.Resolve.Version == nil:
	case c.Resolve.Version.From != "":
		got[resolve.ResultVersionKey] = inputs[c.Resolve.Version.From]
	default:
		in := maps.Clone(inputs)
		if override != "" {
			in[resolve.InputOverrideKey] = override
			override = ""
		}
		var err error
		if got, err = p.versionResolver(name).Resolve(in); err != nil {
			return nil, err
		}
	}

	if c.Resolve.Location == nil {
		return got, nil
	}

	in := maps.Clone(inputs)
	in[resolve.InputVersionKey] = got[resolve.ResultVersionKey]
	if override != "" {
		in[resolve.InputOverrideKey] = override
	}
	r, ok := p.LocationResolvers[name]
	if !ok {
		r = missing{name}
	}
	located, err := r.Resolve(in)
	if err != nil {
		return nil, err
	}
	got[resolve.ResultURLKey] = located[resolve.ResultURLKey]
	return got, nil
}

// missing is the resolver for a component nobody built one for. It fails on
// use, naming the component, so a gap in the wiring is reported where it
// matters instead of dereferencing nil.
type missing struct{ name string }

func (m missing) Resolve(map[string]string) (map[string]string, error) {
	return nil, fmt.Errorf("no resolver was built for %q", m.name)
}

// unsafe matches what an id may not contain. Letters, digits, dot, dash and
// underscore are kept, so an id is safe in a path and in a file name.
var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// combinationID names a combination by the components it is keyed by, as
// <name>-<version> pairs joined with an underscore. The pairs are sorted by
// name so that reordering the subject's component list never changes an id,
// and each is made path-safe so that build metadata such as the plus in
// v1.0.0+build cannot reach a path.
func combinationID(components []ComponentVersion) string {
	sorted := slices.Clone(components)
	slices.SortFunc(sorted, func(a, b ComponentVersion) int { return strings.Compare(a.Name, b.Name) })

	parts := make([]string, len(sorted))
	for i, c := range sorted {
		parts[i] = unsafe.ReplaceAllString(c.Name+"-"+c.Version, "-")
	}
	return strings.Join(parts, "_")
}

func scopeNames(p *Policy) []string {
	names := make([]string, 0, len(p.Spec.Scopes))
	for name := range p.Spec.Scopes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
