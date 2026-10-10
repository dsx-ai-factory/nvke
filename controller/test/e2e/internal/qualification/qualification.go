// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package qualification is the suite's half of the qualification interface.
//
// The values a run qualifies arrive as flags, -e2e.versions and
// -e2e.capabilities, resolved from the qualification plan. This package parses
// those flags and nothing else: it holds no test logic.
package qualification

import (
	"fmt"
	"sort"
	"strings"
)

// Selection is what one qualification run was asked to cover.
type Selection struct {
	// Versions is the component versions the run is qualifying, resolved.
	// The key is the component name as the release policy names it.
	Versions map[string]string

	// Capabilities is the stream's, less the provider: architecture, the
	// Kubernetes minor, a GPU SKU. It describes the configuration rather
	// than the infrastructure, so which site or instance type satisfies it
	// stays in the environment config.
	Capabilities map[string]string

	// Environment is the environment config to run against, named rather
	// than described. This is what keeps a second environment with the same
	// capabilities a config change rather than a tooling change.
	Environment string
}

// Requested reports whether anything was asked for. A plain `make test-e2e`
// sets none of these, and must keep working exactly as it did.
func (s Selection) Requested() bool {
	return len(s.Versions) > 0 || len(s.Capabilities) > 0 || s.Environment != ""
}

// String renders the selection for the suite's log, so a run's output says
// what it was asked to qualify without anyone opening the plan.
func (s Selection) String() string {
	if !s.Requested() {
		return "no qualification selection; running the working tree"
	}
	parts := make([]string, 0, 3)
	if s.Environment != "" {
		parts = append(parts, "environment="+s.Environment)
	}
	if v := join(s.Versions); v != "" {
		parts = append(parts, "versions="+v)
	}
	if c := join(s.Capabilities); c != "" {
		parts = append(parts, "capabilities="+c)
	}
	return strings.Join(parts, " ")
}

// Parse reads the two comma-separated key=value flags.
//
// A malformed pair is an error rather than a skipped entry. These arrive from
// the qualify tool, not from a person, so a pair that does not parse means
// the two sides disagree about the format -- and silently dropping it would
// qualify a different selection than the one recorded.
func Parse(versions, capabilities string) (Selection, error) {
	v, err := pairs(versions)
	if err != nil {
		return Selection{}, fmt.Errorf("-e2e.versions: %w", err)
	}
	c, err := pairs(capabilities)
	if err != nil {
		return Selection{}, fmt.Errorf("-e2e.capabilities: %w", err)
	}
	return Selection{Versions: v, Capabilities: c}, nil
}

func pairs(s string) (map[string]string, error) {
	// Empty rather than nil: Split("", ",") yields one empty entry, which the
	// loop below would reject as malformed, and a caller that ranges over the
	// result should not have to care which it got.
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(entry, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%q is not key=value", entry)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%q appears twice", k)
		}
		out[k] = v
	}
	return out, nil
}

func join(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
