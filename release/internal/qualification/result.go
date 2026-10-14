// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import "time"

// KindQualificationResult is what `qualify result` writes: one stream of one
// combination, after qualify-run executed it.
const KindQualificationResult = "QualificationResult"

// Result is what `qualify result` writes and `qualify publish` reads,
// alongside the JUnit report from the same run. One Result covers exactly one
// (combination, stream) leg -- the unit `qualify result` is invoked for --
// so Status.Combinations always holds exactly one entry, with exactly one
// entry in its own Streams.
type Result struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata,omitempty"`
	Spec       ResultSpec   `json:"spec"`
	Status     ResultStatus `json:"status"`
}

// ResultSpec is the body of a QualificationResult: what was qualified.
type ResultSpec struct {
	Subject string `json:"subject"`

	// Type classifies what was qualified, e.g. "site-controller". Read from
	// the subject's own qualification.yaml (spec.type).
	Type    string `json:"type"`
	Version string `json:"version"`

	// Dependencies are this leg's resolved dependency versions, copied from
	// its plan.json Combination.Dependencies.
	Dependencies []ComponentVersion `json:"dependencies,omitempty"`

	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	Identity ResultIdentity `json:"identity"`
}

// ResultStatus is the body of a QualificationResult: what happened.
type ResultStatus struct {
	Combinations []ResultCombination `json:"combinations"`
}

// ResultCombination is one plan combination's outcome. ID is the plan's own
// identifier (Combination.ID) -- the same string a plan.json names, so a
// result and the plan that produced it join without either side re-deriving
// the other's key.
type ResultCombination struct {
	ID         string             `json:"id"`
	Components []ComponentVersion `json:"components,omitempty"`
	Streams    []ResultStream     `json:"streams"`
}

// ResultStream is one stream's outcome within a combination. Result and
// Summary are read from the Ginkgo report, not re-derived from JUnit --
// there is never a second place a verdict could disagree with the
// authoritative one.
//
// Capabilities is copied from plan.json's Stream.Capabilities at Record time
// so `qualify publish` can build GCS routing (gpu/kube/provider) straight off
// the already-published record -- it does not need to re-read plan.json
// itself.
type ResultStream struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Result  string `json:"result"`
	Summary string `json:"summary"`
	RunURL  string `json:"runUrl,omitempty"`

	Capabilities map[string]string `json:"capabilities,omitempty"`

	// Dependencies are this stream's resolved per-stream dependency
	// versions, copied from its plan.json Stream.Dependencies -- the exact
	// kube patch, OS image build, and similar that ConstraintFrom resolves
	// once per stream rather than once for the whole combination.
	Dependencies []ComponentVersion `json:"dependencies,omitempty"`
}

// ResultIdentity is where this result came from, read from the GitHub
// Actions run's own environment.
type ResultIdentity struct {
	Repository string `json:"repository"`
	RunID      string `json:"runID"`
	RunAttempt string `json:"runAttempt"`
	SHA        string `json:"sha"`
	Workflow   string `json:"workflow"`
	Actor      string `json:"actor"`
}

// NewResult builds a QualificationResult with its type meta set, covering the
// one (combination, stream) leg combination describes.
func NewResult(spec ResultSpec, combination ResultCombination) *Result {
	return &Result{
		APIVersion: APIVersion,
		Kind:       KindQualificationResult,
		Spec:       spec,
		Status:     ResultStatus{Combinations: []ResultCombination{combination}},
	}
}
