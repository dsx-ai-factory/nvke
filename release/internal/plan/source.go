// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package plan

import (
	"fmt"
	"os/exec"
	"strings"
)

// Tags lists the versions a component has been tagged with.
type Tags interface {
	// Versions returns every version of the component, unordered, each with
	// its leading v and without the component prefix.
	Versions(component string) ([]string, error)
}

// History is what the central artifact knows about earlier runs. The Planner
// consults it for both tiers of question it has: whether a version has ever
// been qualified, and what became of one specific cell.
type History interface {
	// Qualified reports whether this version passed at any scope.
	Qualified(component, version string) (bool, error)

	// Outcome reports what became of one cell, OutcomeNone if it never ran.
	Outcome(component, version, combination, stream string) (Outcome, error)
}

// GitTags reads versions from the repository's tags. A component's name is
// its directory and also its tag prefix, because Go requires that of a
// submodule, so one string is enough to find them.
type GitTags struct {
	// Dir is the repository to read. Empty means the working directory.
	Dir string
}

// Versions lists the component's tags.
//
// A component with no tags yields an empty list rather than an error. That is
// a real state -- a new component before its first candidate -- and the
// caller that cares reports it against the dependency that wanted it, which
// is a better message than one naming git.
func (g GitTags) Versions(component string) ([]string, error) {
	cmd := exec.Command("git", "tag", "--list", component+"/v*")
	cmd.Dir = g.Dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing %s tags: %w", component, err)
	}

	var versions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		versions = append(versions, strings.TrimPrefix(line, component+"/"))
	}
	return versions, nil
}

// NoHistory is the central artifact before aggregate has ever written one.
//
// This is not a stub standing in for the real implementation. It is the
// correct answer today: nothing has been qualified, so tier 2 of the `latest`
// selector is empty and every cell is unrun. Reading it from a registry that
// holds nothing would return exactly this.
type NoHistory struct{}

// Qualified reports that nothing is qualified.
func (NoHistory) Qualified(string, string) (bool, error) { return false, nil }

// Outcome reports that no cell has run.
func (NoHistory) Outcome(string, string, string, string) (Outcome, error) {
	return OutcomeNone, nil
}
