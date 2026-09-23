// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package plan turns a trigger into a run list.
//
// The Planner is always invoked for one component. It reads that component's
// qualification spec and the release-wide policy, resolves each dependency's
// selectors, and expands the named scope's selector over the stream list,
// pairing the result with each combination. It is stateless and holds no
// cache, so the same inputs always produce the same plan in the same order.
package plan

import "errors"

// errNoCandidate is returned when no version survives a dependency's
// constraint. It fails here, before anything is scheduled, rather than
// producing a plan nothing can run.
var errNoCandidate = errors.New("no version satisfies the constraint")

// Plan is what `qualify plan` writes and every later verb reads.
type Plan struct {
	Component    string        `json:"component"`
	Version      string        `json:"version"`
	Scope        string        `json:"scope"`
	Combinations []Combination `json:"combinations"`
}

// Combination is one resolved set of dependency versions, to be qualified
// against every stream in the scope.
type Combination struct {
	ID           string       `json:"id"`
	Dependencies []Dependency `json:"dependencies"`
	Streams      []Stream     `json:"streams"`
}

// Dependency is one dependency with its selector resolved to a version.
type Dependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Stream is one cell of the matrix.
type Stream struct {
	Name string `json:"name"`

	// Skip says why the Planner decided against running this cell, and is
	// empty when it will run. Skipped cells stay in the file rather than
	// being dropped, so a plan covering four of six streams says so instead
	// of looking like a smaller matrix.
	Skip Outcome `json:"skip,omitempty"`
}

// Outcome is what became of a cell in an earlier run.
type Outcome string

// The outcomes a cell can carry. Passed and Failed are skipped on a rerun,
// because running the same candidate again proves nothing either way. Error
// and Unschedulable are not, because the answer is still unknown.
const (
	OutcomeNone          Outcome = ""
	OutcomePassed        Outcome = "passed"
	OutcomeFailed        Outcome = "failed"
	OutcomeError         Outcome = "error"
	OutcomeUnschedulable Outcome = "unschedulable"
)

// skippable reports whether an earlier outcome settles the question.
func (o Outcome) skippable() bool {
	return o == OutcomePassed || o == OutcomeFailed
}
