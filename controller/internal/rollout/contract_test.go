// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

// TestOutcome_String pins the human-readable name of every Outcome,
// including the two states added as the framework evolved: the
// fail-closed zero-value sentinel (Unspecified) and the
// preflight-skip state (SkippedPreflight). A drift here would corrupt
// every structured log line and dashboard the executors emit.
func TestOutcome_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		outcome rollout.Outcome
		want    string
	}{
		{rollout.OutcomeUnspecified, "Unspecified"},
		{rollout.OutcomeApplied, "Applied"},
		{rollout.OutcomeUnchanged, "Unchanged"},
		{rollout.OutcomeFailedRestored, "FailedRestored"},
		{rollout.OutcomeFailedNotRestored, "FailedNotRestored"},
		{rollout.OutcomeSkippedPreflight, "SkippedPreflight"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.outcome.String())
	}

	// An out-of-range value is reported with its raw int, distinct from
	// the in-range Unspecified sentinel.
	assert.Equal(t, "Unknown(99)", rollout.Outcome(99).String())
}

// TestRolloutError_Error covers the four formatting branches of
// RolloutError.Error: a named cluster phase (preflight/guard), an
// unnamed cluster phase (Validate/BuildApplyToNodePayload/PostCondition),
// per-node failures, and the bare status fallback.
func TestRolloutError_Error(t *testing.T) {
	t.Parallel()

	t.Run("named cluster phase", func(t *testing.T) {
		t.Parallel()
		e := &rollout.RolloutError{
			Status:       rollout.ResultFailed,
			PhaseFailure: &rollout.PhaseFailure{Phase: rollout.PhaseClusterPreflight, Name: "etcd-quorum-healthy", Err: "not quorate"},
		}
		assert.Equal(t, `rollout Failed: ClusterPreflight "etcd-quorum-healthy": not quorate`, e.Error())
	})

	t.Run("unnamed cluster phase", func(t *testing.T) {
		t.Parallel()
		e := &rollout.RolloutError{
			Status:       rollout.ResultFailed,
			PhaseFailure: &rollout.PhaseFailure{Phase: rollout.PhaseBuildApplyToNodePayload, Err: "render failed"},
		}
		assert.Equal(t, "rollout Failed: BuildApplyToNodePayload: render failed", e.Error())
	})

	t.Run("per-node failures", func(t *testing.T) {
		t.Parallel()
		e := &rollout.RolloutError{
			Status: rollout.ResultRolledBack,
			NodesFailed: []rollout.NodeResult{
				{NodeName: "cp-1", Outcome: rollout.OutcomeFailedNotRestored},
				{NodeName: "cp-2", Outcome: rollout.OutcomeFailedNotRestored},
			},
		}
		assert.Equal(t, "rollout RolledBack (2 node(s) failed)", e.Error())
	})

	t.Run("bare status fallback", func(t *testing.T) {
		t.Parallel()
		e := &rollout.RolloutError{Status: rollout.ResultFailed}
		assert.Equal(t, "rollout Failed", e.Error())
	})
}

// TestResult_Err verifies the success/failure collapse: Completed and
// AlreadySatisfied return nil; every other status (including a
// zero-value Status from a malformed Result) returns a *RolloutError
// carrying the phase/node detail, so `if err := result.Err()` can never
// treat an unexpected status as success.
func TestResult_Err(t *testing.T) {
	t.Parallel()

	assert.NoError(t, rollout.Result{Status: rollout.ResultCompleted}.Err())
	assert.NoError(t, rollout.Result{Status: rollout.ResultAlreadySatisfied}.Err())

	t.Run("failure carries detail via errors.As", func(t *testing.T) {
		t.Parallel()
		nodeID := uuid.New()
		res := rollout.Result{
			Status: rollout.ResultRolledBack,
			Nodes:  []rollout.NodeResult{{NodeID: nodeID, NodeName: "cp-1", Outcome: rollout.OutcomeFailedNotRestored}},
		}
		err := res.Err()
		require.Error(t, err)
		var re *rollout.RolloutError
		require.ErrorAs(t, err, &re)
		assert.Equal(t, rollout.ResultRolledBack, re.Status)
		require.Len(t, re.NodesFailed, 1)
		assert.Equal(t, nodeID, re.NodesFailed[0].NodeID)
	})

	t.Run("zero-value status is not success", func(t *testing.T) {
		t.Parallel()
		err := rollout.Result{}.Err()
		require.Error(t, err, "a malformed Result with an empty Status must not be treated as success")
	})
}
