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
func Check(p *Policy, r *Registry, s *Subject, resolvers map[string]resolve.Resolver) error {
	if _, ok := r.Component(s.Spec.Subject); !ok {
		return fmt.Errorf("subject %q is not a component in the registry", s.Spec.Subject)
	}
	for _, c := range s.Spec.Components {
		if _, ok := r.Component(c.Name); !ok {
			return fmt.Errorf("%s lists %q, which is not a component in the registry", s.Spec.Subject, c.Name)
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
		if err := checkInputs(p, s.Spec.Subject, d, c.Resolve.Strategy, resolvers[d.Name]); err != nil {
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

// checkInputs asks the dependency's resolver whether what the subject declares
// for it is something it can resolve, once for each stream when the dependency
// is per stream. A resolver that was not built is left to the planner, which
// reports it where it is used.
func checkInputs(p *Policy, subject string, d SubjectDependency, strategy string, r resolve.Resolver) error {
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
		if r == nil {
			continue
		}
		if err := resolve.Validate(r, in); err != nil {
			return fmt.Errorf("%s: %q (%s)%s: %w", subject, d.Name, strategy, where, err)
		}
	}
	return nil
}
