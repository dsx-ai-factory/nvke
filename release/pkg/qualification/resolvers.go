// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/release/pkg/resolve"
)

// VersionFactory builds the version resolver for a strategy a caller adds to
// the built-in ones. with is the strategy's configuration from the registry,
// and repo is the tree the version resolvers read.
type VersionFactory func(with map[string]string, repo string) (resolve.Resolver, error)

// LocationFactory builds the location resolver for a strategy a caller adds to
// the built-in ones. with is the strategy's configuration from the registry,
// with the source it names already expanded to its base.
type LocationFactory func(with map[string]string, opts resolve.Options) (resolve.Resolver, error)

// Strategies are the strategies a caller adds to the built-in ones, keyed by
// the name a registry uses for them. A name belongs to one half, and cannot be
// a name the built-in strategies already use.
type Strategies struct {
	Version  map[string]VersionFactory
	Location map[string]LocationFactory
}

// validate rejects an addition that would shadow a built-in strategy, appear
// in both halves, or have no factory.
func (s Strategies) validate() error {
	for name, f := range s.Version {
		if f == nil {
			return fmt.Errorf("version strategy %q has no factory", name)
		}
		if resolve.IsVersionStrategy(name) || resolve.IsLocationStrategy(name) {
			return fmt.Errorf("version strategy %q is built in and cannot be replaced", name)
		}
		if _, both := s.Location[name]; both {
			return fmt.Errorf("strategy %q is added as both a version and a location strategy", name)
		}
	}
	for name, f := range s.Location {
		if f == nil {
			return fmt.Errorf("location strategy %q has no factory", name)
		}
		if resolve.IsVersionStrategy(name) || resolve.IsLocationStrategy(name) {
			return fmt.Errorf("location strategy %q is built in and cannot be replaced", name)
		}
	}
	return nil
}

// newVersion builds the version resolver for strategy, from the caller's
// additions when it names one and from the built-in strategies otherwise.
func (s Strategies) newVersion(strategy string, with map[string]string, repo string) (resolve.Resolver, error) {
	f, ok := s.Version[strategy]
	if !ok {
		return resolve.NewVersion(strategy, with, repo)
	}
	r, err := f(with, repo)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("strategy %q built no resolver", strategy)
	}
	return r, nil
}

// newLocation builds the location resolver for strategy, from the caller's
// additions when it names one and from the built-in strategies otherwise.
func (s Strategies) newLocation(strategy string, with map[string]string, opts resolve.Options) (resolve.Resolver, error) {
	f, ok := s.Location[strategy]
	if !ok {
		return resolve.NewLocation(strategy, with, opts)
	}
	r, err := f(with, opts)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("strategy %q built no resolver", strategy)
	}
	return r, nil
}

// BuildResolvers makes a resolver for each half each registry component
// declares, in the maps a Planner takes. This is the one place that names a
// strategy, which is what lets the planner be tested with fakes. extra holds
// the strategies the caller adds to the built-in ones, and may be the zero
// value.
//
// A half a component does not declare has no entry, which is how the planner
// tells "this entry has no location" from "its location failed to build".
func BuildResolvers(reg *Registry, root string, opts resolve.Options, extra Strategies) (version, location map[string]resolve.Resolver, err error) {
	if err := extra.validate(); err != nil {
		return nil, nil, err
	}
	version = make(map[string]resolve.Resolver, len(reg.Spec.Components))
	location = make(map[string]resolve.Resolver, len(reg.Spec.Components))
	for _, c := range reg.Spec.Components {
		// A version taken from another entry names no strategy, so there is
		// nothing to build and the planner reads it from the inputs.
		if v := c.Resolve.Version; v != nil && v.Strategy != "" {
			r, err := extra.newVersion(v.Strategy, reg.StrategyConfig(v), root)
			if err != nil {
				return nil, nil, fmt.Errorf("component %s, version: %w", c.Name, err)
			}
			version[c.Name] = r
		}
		if l := c.Resolve.Location; l != nil {
			r, err := extra.newLocation(l.Strategy, reg.StrategyConfig(l), opts)
			if err != nil {
				return nil, nil, fmt.Errorf("component %s, location: %w", c.Name, err)
			}
			location[c.Name] = r
		}
	}
	return version, location, nil
}
