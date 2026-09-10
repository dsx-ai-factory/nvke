// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"fmt"
	"sort"
)

// Phases groups the composition's components into install order.
//
// Helm does not order subcharts and never waits mid-release, so installing the
// whole chart in one pass races: spire's Certificate reaches cert-manager's
// admission webhook before it serves, which is a hard failure rather than a
// retry, and temporal's schema jobs reach for a Postgres that may not be
// accepting connections. Installing phase by phase with --wait reproduces the
// ordering controller/Tiltfile gets from resource_deps.
//
// The grouping is a Kahn levelisation of the dependsOn graph: everything with no
// unsatisfied dependency goes in the current phase, then those components are
// removed and the next phase is taken. Components in one phase are independent
// of each other, so they install concurrently.
//
// Phases are derived rather than authored precisely so they cannot drift from
// the edges they are supposed to encode.
func (r *Recipe) Phases() ([]Phase, error) {
	remaining := map[string][]string{}
	order := make([]string, 0, len(r.Composition.Spec.ComponentRefs)) // registry order, so ties break deterministically
	for _, ref := range r.Composition.Spec.ComponentRefs {
		remaining[ref.Name] = append([]string(nil), ref.DependsOn...)
		order = append(order, ref.Name)
	}

	var phases []Phase
	done := map[string]bool{}
	for len(remaining) > 0 {
		var ready []string
		for _, name := range order {
			deps, ok := remaining[name]
			if !ok {
				continue
			}
			satisfied := true
			for _, d := range deps {
				if !done[d] {
					satisfied = false
					break
				}
			}
			if satisfied {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			stuck := make([]string, 0, len(remaining))
			for name := range remaining {
				stuck = append(stuck, name)
			}
			sort.Strings(stuck)
			return nil, fmt.Errorf("dependsOn has a cycle among %v", stuck)
		}
		phases = append(phases, Phase{Name: phaseName(len(phases), ready), Components: ready})
		for _, name := range ready {
			delete(remaining, name)
			done[name] = true
		}
	}
	return phases, nil
}

// phaseName labels a phase by what it contains. The names are cosmetic -- the
// installer drives off the component lists -- but they appear in install logs,
// so "identity" reads better than "phase-2".
func phaseName(index int, components []string) string {
	known := map[string]string{
		"postgresql": "datastore",
		"spire":      "identity",
		"controller": "controller",
	}
	for _, c := range components {
		if n, ok := known[c]; ok {
			return n
		}
	}
	return fmt.Sprintf("phase-%d", index+1)
}
