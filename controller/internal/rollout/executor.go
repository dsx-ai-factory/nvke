// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package rollout

import (
	"fmt"

	"github.com/google/uuid"
)

// Phase labels for cluster-scoped executor phases.
type Phase string

const (
	PhaseSatisfactionCheck       Phase = "SatisfactionCheck"
	PhaseValidate                Phase = "Validate"
	PhaseMinHealthyReplicas      Phase = "MinHealthyReplicasFeasibility"
	PhaseClusterPreflight        Phase = "ClusterPreflight"
	PhaseClusterGuard            Phase = "ClusterGuard"
	PhaseBuildApplyToNodePayload Phase = "BuildApplyToNodePayload"
	PhasePostCondition           Phase = "ClusterPostCondition"
)

// ResultStatus is the four-way status of a complete rollout execution.
type ResultStatus string

const (
	ResultCompleted        ResultStatus = "Completed"
	ResultAlreadySatisfied ResultStatus = "AlreadySatisfied" // SatisfactionChecker returned true; per-node loop skipped

	// ResultFailed: halted without cascade — OutcomeFailedRestored, preflight
	// failure, or cluster-scoped phase abort (see Result.PhaseFailure).
	ResultFailed ResultStatus = "Failed"

	// ResultRolledBack: cascade ran after OutcomeFailedNotRestored or quorum violation.
	ResultRolledBack ResultStatus = "RolledBack"
)

// PhaseFailure is set on Result only for cluster-scoped phase aborts;
// node-level failures live in Result.Nodes (see Result.Failed).
type PhaseFailure struct {
	Phase Phase
	Name  string
	Err   string
}

// Result is the outcome of a complete rollout execution. Every touched
// node is recorded once in Nodes, in apply order, tagged with its
// per-node Outcome; partition with Applied / Unchanged / Failed.
// PhaseFailure is set only for cluster-scoped aborts. Use Err for a
// success/failure check.
type Result struct {
	Status       ResultStatus
	PhaseFailure *PhaseFailure
	Nodes        []NodeResult
}

// Applied returns the nodes that applied the change (OutcomeApplied).
func (r Result) Applied() []NodeResult {
	return filterNodes(r.Nodes, func(o Outcome) bool { return o == OutcomeApplied })
}

// Unchanged returns the nodes already in the desired state (OutcomeUnchanged).
func (r Result) Unchanged() []NodeResult {
	return filterNodes(r.Nodes, func(o Outcome) bool { return o == OutcomeUnchanged })
}

// Failed returns the nodes that did not apply successfully — every
// Outcome other than Applied/Unchanged (FailedRestored, FailedNotRestored,
// SkippedPreflight, and the Unspecified zero value).
func (r Result) Failed() []NodeResult {
	return filterNodes(r.Nodes, func(o Outcome) bool {
		return o != OutcomeApplied && o != OutcomeUnchanged
	})
}

func filterNodes(nodes []NodeResult, keep func(Outcome) bool) []NodeResult {
	var out []NodeResult
	for _, n := range nodes {
		if keep(n.Outcome) {
			out = append(out, n)
		}
	}
	return out
}

// NodeResult is the per-node outcome recorded in Result. Err is a
// stringified error (the executor crosses the Temporal activity boundary,
// which cannot serialize error values); empty unless Outcome is a failure.
type NodeResult struct {
	NodeID   uuid.UUID
	NodeName string
	Outcome  Outcome
	Err      string
}

// RolloutError is the error-interface view of a non-success Result,
// returned by Result.Err so callers can treat a failed rollout as a plain
// Go error. It carries only the projection needed to explain the failure
// — the Status, an optional cluster-scoped PhaseFailure, and the failed
// nodes — not the full per-node breakdown (that stays on Result). Inspect
// PhaseFailure / NodesFailed via errors.As.
type RolloutError struct {
	Status       ResultStatus
	PhaseFailure *PhaseFailure
	NodesFailed  []NodeResult
}

func (e *RolloutError) Error() string {
	if e.PhaseFailure != nil {
		if e.PhaseFailure.Name != "" {
			return fmt.Sprintf("rollout %s: %s %q: %s",
				e.Status, e.PhaseFailure.Phase, e.PhaseFailure.Name, e.PhaseFailure.Err)
		}
		return fmt.Sprintf("rollout %s: %s: %s",
			e.Status, e.PhaseFailure.Phase, e.PhaseFailure.Err)
	}
	if len(e.NodesFailed) == 0 {
		return fmt.Sprintf("rollout %s", e.Status)
	}
	return fmt.Sprintf("rollout %s (%d node(s) failed)", e.Status, len(e.NodesFailed))
}

// Err returns nil for Completed and AlreadySatisfied; otherwise *RolloutError.
func (r Result) Err() error {
	if r.Status == ResultCompleted || r.Status == ResultAlreadySatisfied {
		return nil
	}
	return &RolloutError{
		Status:       r.Status,
		PhaseFailure: r.PhaseFailure,
		NodesFailed:  r.Failed(),
	}
}
