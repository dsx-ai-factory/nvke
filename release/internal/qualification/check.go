// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import "fmt"

// Check holds the three files to each other. Each is valid alone, and this is
// where a name one of them uses is required to exist in another.
//
// Every name a subject lists has to be a registered component, because one
// that is not could never be resolved. Every constraint that takes a stream's
// capability has to name one that every stream declares, because a stream
// without it would have nothing to resolve against.
func Check(p *Policy, r *Registry, s *Subject) error {
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
		if _, ok := r.Component(d.Name); !ok {
			return fmt.Errorf("%s depends on %q, which is not a component in the registry", s.Spec.Subject, d.Name)
		}
		if !d.perStream() {
			continue
		}
		for _, stream := range p.Spec.Streams {
			if _, ok := stream.Capabilities[d.Constraint.Capability]; !ok {
				return fmt.Errorf("%s: %q takes capability %q, which stream %q does not declare",
					s.Spec.Subject, d.Name, d.Constraint.Capability, stream.Name)
			}
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
