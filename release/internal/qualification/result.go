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
// so Status holds exactly one Combination, with exactly one Stream of its
// own.
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
	Version string `json:"version"`

	// Dependencies are this leg's resolved dependency versions, copied from
	// its plan.json Combination.Dependencies.
	Dependencies []ComponentVersion `json:"dependencies,omitempty"`

	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	Identity RunIdentity `json:"identity"`
}

// ResultStatus is the body of a QualificationResult: what happened. One
// Combination, not a slice: a Result covers exactly one (combination,
// stream) leg, so a slice here would let a value disagree with its own
// invariant instead of making the shape itself unrepresentable.
type ResultStatus struct {
	Combination ResultCombination `json:"combination"`
}

// ResultCombination is the plan combination this leg's result came from. ID
// is the plan's own identifier (Combination.ID) -- the same string a
// plan.json names, so a result and the plan that produced it join without
// either side re-deriving the other's key.
type ResultCombination struct {
	ID         string             `json:"id"`
	Components []ComponentVersion `json:"components,omitempty"`
	Stream     ResultStream       `json:"stream"`
}

// ResultPassed, ResultFailed and ResultErrored are the three values
// ResultStream.Result takes. ResultErrored is distinct from ResultFailed: it
// means the suite never ran at all -- a Forge provisioning failure, a wedged
// runner -- so a dashboard can tell a product regression (ResultFailed) apart
// from infrastructure noise (ResultErrored) instead of both reading as the
// same red entry.
const (
	ResultPassed  = "passed"
	ResultFailed  = "failed"
	ResultErrored = "errored"
)

// ResultStream is one stream's outcome within a combination. Result and
// Summary are read from the Ginkgo report, not re-derived from JUnit --
// there is never a second place a verdict could disagree with the
// authoritative one.
//
// Capabilities is copied from plan.json's Stream.Capabilities at Record time
// so `qualify publish` can build GCS routing (gpu/kube/provider) straight off
// the already-published record -- it does not need to re-read plan.json
// itself.
//
// Suite is the Ginkgo report's own SuiteDescription (e.g. "NVKE E2E Suite"),
// not re-derived or guessed: it is only ever set when a report was actually
// read, since that is the only place this name exists. A result with no
// report (Result == ResultErrored) carries no Suite, and `qualify publish`
// does not publish that leg at all -- there is nothing meaningful to tab it
// under, since Ginkgo never ran far enough to say which suite this was.
type ResultStream struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Result  string `json:"result"` // ResultPassed, ResultFailed, or ResultErrored
	Summary string `json:"summary"`
	Suite   string `json:"suite,omitempty"`
	RunURL  string `json:"runUrl,omitempty"`

	Capabilities map[string]string `json:"capabilities,omitempty"`

	// Dependencies are this stream's resolved per-stream dependency
	// versions, copied from its plan.json Stream.Dependencies -- the exact
	// kube patch, OS image build, and similar that ConstraintFrom resolves
	// once per stream rather than once for the whole combination.
	Dependencies []ComponentVersion `json:"dependencies,omitempty"`
}

// RunIdentity is the run this result came from, read from the GitHub
// Actions run's own environment -- not part of the result itself, which is
// why it does not carry the Result prefix its siblings ResultSpec and
// ResultStatus do.
type RunIdentity struct {
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
		Status:     ResultStatus{Combination: combination},
	}
}

// LoadResult reads a Result written by `qualify result`, the same way
// LoadSubject reads its own file: readStrict rejects a record carrying an
// unknown field rather than silently dropping it, and checkTypeMeta rejects
// one whose apiVersion or kind do not match what `qualify publish` expects,
// rather than only checking kind.
func LoadResult(path string) (*Result, error) {
	var r Result
	if err := readStrict(path, &r); err != nil {
		return nil, err
	}
	if err := checkTypeMeta(path, r.APIVersion, r.Kind, KindQualificationResult); err != nil {
		return nil, err
	}
	return &r, nil
}
