// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// Check holds the three files to each other. Each is valid alone, and this is
// where a name one of them uses is required to exist in another.
//
// Every name a subject lists has to be a registered component, because one
// that is not could never be resolved. Every constraint that takes a stream's
// capability has to name one that every stream declares, because a stream
// without it would have nothing to resolve against. What a dependency declares
// is then held to its resolver, which knows what its strategy takes and needs.
func Check(p *Policy, r *Registry, s *Subject, version, location map[string]resolve.Resolver) error {
	if _, ok := r.Component(s.Spec.Subject); !ok {
		return fmt.Errorf("subject %q is not a component in the registry", s.Spec.Subject)
	}

	// Which names a version may be taken from, and which of those have one to
	// give. A component is resolved before any dependency and its result is
	// offered to every one of them, which a dependency's result is not. A
	// component that resolves only a location has nothing to lend, and the two
	// are separate mistakes with separate fixes.
	listed := map[string]bool{s.Spec.Subject: true}
	lends := map[string]bool{}
	for _, c := range s.Spec.Components {
		listed[c.Name] = true
	}
	for name := range listed {
		if rc, ok := r.Component(name); ok && rc.Resolve.Version != nil {
			lends[name] = true
		}
	}

	for _, c := range s.Spec.Components {
		rc, ok := r.Component(c.Name)
		if !ok {
			return fmt.Errorf("%s lists %q, which is not a component in the registry", s.Spec.Subject, c.Name)
		}
		if from := versionFrom(rc); from != "" {
			return fmt.Errorf("%s lists %q as a component, but it takes its version from %q, and a "+
				"component is resolved before anything it could take one from", s.Spec.Subject, c.Name, from)
		}
		for _, sel := range c.Selectors {
			if sel != SelectorLatest {
				return fmt.Errorf("%s: component %q has selector %q, the only selector is %q",
					s.Spec.Subject, c.Name, sel, SelectorLatest)
			}
		}
	}
	for _, d := range s.Spec.Dependencies {
		c, ok := r.Component(d.Name)
		if !ok {
			return fmt.Errorf("%s depends on %q, which is not a component in the registry", s.Spec.Subject, d.Name)
		}
		if from := versionFrom(c); from != "" {
			switch {
			case !listed[from]:
				return fmt.Errorf("%s: %q takes its version from %q, which the subject does not list as a "+
					"component, so there would be no version to take", s.Spec.Subject, d.Name, from)
			case !lends[from]:
				return fmt.Errorf("%s: %q takes its version from %q, which resolves only a location, "+
					"so it has no version to give", s.Spec.Subject, d.Name, from)
			}
		}
		if err := checkInputs(p, s.Spec.Subject, d, c, version[d.Name], location[d.Name]); err != nil {
			return err
		}
	}

	// A name the policy has never heard of is a typo, not a narrowing, and
	// intersecting would swallow it as "selects nothing".
	known := map[string]bool{}
	for _, stream := range p.Spec.Streams {
		known[stream.Name] = true
	}
	for _, want := range s.Spec.Streams {
		if !known[want] {
			return fmt.Errorf("%s names stream %q, which the policy does not define", s.Spec.Subject, want)
		}
	}
	return nil
}

// checkInputs asks the dependency's resolvers whether what the subject declares
// for it is something they can resolve, once for each stream when the
// dependency is per stream. A resolver that was not built is left to the
// planner, which reports it where it is used.
//
// Both halves are asked, because a declared input belongs to whichever one
// consumes it: a constraint is read by lockedPackage on the version side and by
// osImage on the location side, and a value by url on the location side. The
// half that rejects it is the one the error names.
func checkInputs(p *Policy, subject string, d SubjectDependency, c Component, version, location resolve.Resolver) error {
	halves := []struct {
		rule *Rule
		r    resolve.Resolver
	}{{c.Resolve.Version, version}, {c.Resolve.Location, location}}

	streams := []*StreamDefinition{nil}
	if d.perStream() {
		streams = streams[:0]
		for i := range p.Spec.Streams {
			streams = append(streams, &p.Spec.Streams[i])
		}
	}

	for _, stream := range streams {
		where := ""
		if stream != nil {
			where = fmt.Sprintf(", stream %q", stream.Name)
		}

		in, err := d.inputs(stream)
		if err != nil {
			return fmt.Errorf("%s: %q%s: %w", subject, d.Name, where, err)
		}
		for _, half := range halves {
			if half.r == nil {
				continue
			}
			if err := resolve.Validate(half.r, in); err != nil {
				return fmt.Errorf("%s: %q (%s)%s: %w", subject, d.Name, half.rule.Strategy, where, err)
			}
		}
	}
	return nil
}

// versionFrom is the entry a component takes its version from, or empty when it
// resolves one of its own.
func versionFrom(c Component) string {
	if c.Resolve.Version == nil {
		return ""
	}
	return c.Resolve.Version.From
}
