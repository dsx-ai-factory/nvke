// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/temporal"
)

// activityCtx installs a StartToCloseTimeout and MaximumAttempts=1.
// Without the retry cap, every `.Once()` mock expectation would be
// multiplied by Temporal's default unbounded retry.
func activityCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &sdktemporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
}

// fakeCluster is a minimal rollout.Cluster.
type fakeCluster struct {
	id  uuid.UUID
	cps []rollout.Node
}

func (c *fakeCluster) ID() uuid.UUID                     { return c.id }
func (c *fakeCluster) Name() string                      { return "fake" }
func (c *fakeCluster) KubernetesVersion() string         { return "v1.30.0" }
func (c *fakeCluster) ControlPlaneVIP() string           { return "10.0.0.1:6443" }
func (c *fakeCluster) ControlPlaneNodes() []rollout.Node { return c.cps }
func (c *fakeCluster) Nodes() []rollout.Node             { return c.cps }

// fakeNode is a minimal rollout.Node.
type fakeNode struct {
	id   uuid.UUID
	name string
}

func (n fakeNode) ID() uuid.UUID          { return n.id }
func (n fakeNode) Name() string           { return n.name }
func (n fakeNode) Role() rollout.NodeRole { return rollout.NodeRoleControlPlane }
func (n fakeNode) AgentEndpoint() string  { return "10.0.0.1:50051" }

// noopCore is a minimal Operation for tests where the operation
// never actually runs (guard short-circuits, preflight aborts).
type noopCore struct{}

func (n *noopCore) Name() string { return "noop" }
func (n *noopCore) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

// IsSatisfied's business error packs into resp.IsSatErr and surfaces
// as PhaseFailure{Phase: PhaseSatisfactionCheck}.
func TestTemporalExecute_IsSatisfiedBusinessError_AbortsWithPhaseFailure(t *testing.T) {
	t.Parallel()

	env := newGuardTestEnv()

	// Business failure: activity ran, IsSatisfied reported an error
	// via resp.IsSatErr. SDK boundary sees no error.
	env.OnActivity(temporal.ActivityIsSatisfied, mock.Anything, mock.Anything).
		Return(&temporal.IsSatisfiedResponse{
			IsSatErr: "is_satisfied: tenant kube-apiserver unreachable",
		}, nil).Once()

	// Downstream activities must NOT be dispatched — we aborted at
	// satisfaction-check.
	env.OnActivity(temporal.ActivityValidate, mock.Anything, mock.Anything).
		Return(nil).Maybe()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeUnchanged}, nil).Maybe()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: uuid.New(), name: "cp-0"}}}

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&satisfactionCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"business failure must surface as outcome-class, not workflow error")

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	require.NotNil(t, result.PhaseFailure)
	assert.Equal(t, rollout.PhaseSatisfactionCheck, result.PhaseFailure.Phase)
	assert.Contains(t, result.PhaseFailure.Err, "tenant kube-apiserver unreachable")
}

// BuildApplyToNodePayload's business error packs into resp.BuildErr and
// surfaces as PhaseFailure{Phase: PhaseBuildApplyToNodePayload}.
func TestTemporalExecute_BuildApplyToNodePayloadBusinessError_AbortsWithPhaseBuildApplyToNodePayload(t *testing.T) {
	t.Parallel()

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityBuildApplyToNodePayload, mock.Anything, mock.Anything).
		Return(&temporal.BuildApplyToNodePayloadResponse{
			BuildErr: "build_payload: SPIRE OIDC ConfigMap not found",
		}, nil).Once()

	// ApplyToNode must NOT be dispatched — we aborted at payload build.
	applyCallCount := 0
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { applyCallCount++ }).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeUnchanged}, nil).
		Maybe()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: uuid.New(), name: "cp-0"}}}

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&payloadCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status,
		"BuildApplyToNodePayload business failure surfaces as ResultFailed with PhaseFailure{Phase: PhaseBuildApplyToNodePayload}")
	require.NotNil(t, result.PhaseFailure)
	assert.Equal(t, rollout.PhaseBuildApplyToNodePayload, result.PhaseFailure.Phase)
	assert.Contains(t, result.PhaseFailure.Err, "SPIRE OIDC ConfigMap not found")
	assert.Zero(t, applyCallCount, "ApplyToNode must not run after BuildApplyToNodePayload business failure")
}

// satisfactionCore is an Operation + SatisfactionChecker for tests
// that exercise the IsSatisfied dispatch. The activity body's behavior
// is what the test asserts; this core just exists so HasSatisfactionChecker
// returns true on the wrapped Operation.
type satisfactionCore struct{}

func (s *satisfactionCore) Name() string { return "satisfaction-test" }
func (s *satisfactionCore) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

func (s *satisfactionCore) IsSatisfied(_ context.Context, _ rollout.Cluster) (bool, error) {
	return false, nil
}

// payloadCore is the sibling for BuildApplyToNodePayload dispatch tests.
type payloadCore struct{}

func (a *payloadCore) Name() string { return "payload-test" }
func (a *payloadCore) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

func (a *payloadCore) BuildApplyToNodePayload(_ context.Context, _ rollout.Cluster) (rollout.ApplyToNodePayload, error) {
	return rollout.ApplyToNodePayload("payload"), nil
}

// ClusterPreflight failure → ResultFailed + PhaseFailure, no cascade.
func TestExecute_ClusterPreflightFails_ReturnsFailureResult(t *testing.T) {
	t.Parallel()

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityClusterPreflight, mock.Anything, mock.Anything).
		Return(errors.New("etcd not healthy")).Once()

	cluster := &fakeCluster{id: uuid.New(), cps: nil}

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			ClusterPreflights: []rollout.ClusterPreflightRef{{Name: "check-etcd"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "preflight failure is outcome-class, not executor-internal")

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	assert.Empty(t, result.Failed(), "cluster-preflight failure must not synthesize Failed() entries")
	require.NotNil(t, result.PhaseFailure)
	assert.Equal(t, rollout.PhaseClusterPreflight, result.PhaseFailure.Phase)
	assert.Equal(t, "check-etcd", result.PhaseFailure.Name)
}

// NodePreflight failure halts without cascade; one Failed() entry.
func TestExecute_NodePreflightFails_HaltsWithoutCascade(t *testing.T) {
	t.Parallel()

	node1ID := uuid.New()
	node2ID := uuid.New()
	cluster := &fakeCluster{
		id:  uuid.New(),
		cps: []rollout.Node{fakeNode{id: node1ID, name: "cp-1"}, fakeNode{id: node2ID, name: "cp-2"}},
	}

	env := newGuardTestEnv()

	// NodePreflight fails on node-1 (first call). The executor returns
	// immediately; node-2 is never reached and no rollback fires.
	env.OnActivity(temporal.ActivityNodePreflight, mock.Anything, mock.Anything).
		Return(errors.New("kubelet not ready")).Once()

	// Track whether ApplyToNode or RollbackNode are accidentally called.
	applyCallCount := 0
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { applyCallCount++ }).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).
		Maybe()

	rollbackCallCount := 0
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { rollbackCallCount++ }).
		Return(nil).
		Maybe()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodePreflights: []rollout.NodePreflightRef{{Name: "check-kubelet"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "preflight failure is outcome-class, not executor-internal")

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	require.Len(t, result.Failed(), 1, "only node-1 should appear in Failed()")
	assert.Equal(t, node1ID, result.Failed()[0].NodeID)
	assert.Equal(t, rollout.OutcomeSkippedPreflight, result.Failed()[0].Outcome,
		"preflight-rejected node was never touched; Outcome must be SkippedPreflight, not FailedRestored")
	assert.Zero(t, applyCallCount, "ApplyToNode must not be called when NodePreflight fails")
	assert.Zero(t, rollbackCallCount, "RollbackNode must not be called (no cascade on NodePreflight failure)")
}

// The three cleanup-mutation activities (RollbackNode,
// NodeGuardPostApply, ClusterGuardPostRollout) must return
// non-retryable errors — see nonRetryableCleanupError.
func TestCleanupActivities_ReturnNonRetryableApplicationError(t *testing.T) {
	t.Parallel()

	a := temporal.NewActivities(&nopLoader{}, rollout.NewRegistry())

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "NodeGuardPostApply",
			call: func() error {
				return a.NodeGuardPostApply(context.Background(), &temporal.NodeGuardPostApplyRequest{
					GuardName: "silence",
					ClusterID: uuid.New(),
					NodeID:    uuid.New(),
					Outcome:   rollout.OutcomeApplied,
					// Empty State.GuardName → triggers the wrapped
					// validation error path.
					State: rollout.GuardState{},
				})
			},
		},
		{
			name: "ClusterGuardPostRollout",
			call: func() error {
				return a.ClusterGuardPostRollout(context.Background(), &temporal.ClusterGuardPostRolloutRequest{
					GuardName: "silence",
					ClusterID: uuid.New(),
					Result:    rollout.Result{Status: rollout.ResultCompleted},
					State:     rollout.GuardState{},
				})
			},
		},
		{
			name: "RollbackNode",
			call: func() error {
				// Unknown operation name → resolve fails →
				// returns wrapped non-retryable.
				return a.RollbackNode(context.Background(), &temporal.RollbackRequest{
					OperationName: "no-such-op",
					ClusterID:     uuid.New(),
					NodeID:        uuid.New(),
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call()
			require.Error(t, err)
			var appErr *sdktemporal.ApplicationError
			require.ErrorAs(t, err, &appErr,
				"%s must return *temporal.ApplicationError (was: %T)", tc.name, err)
			assert.True(t, appErr.NonRetryable(),
				"%s must return a NON-retryable application error so the SDK does not re-dispatch the cleanup",
				tc.name)
		})
	}
}

// newGuardTestEnv returns a Temporal test environment with the rollout
// temporal activities pre-registered so OnActivity can layer mocks on
// top. Mirrors the pattern in pkg/workflows/rollout_dry_run.
func newGuardTestEnv() *testsuite.TestWorkflowEnvironment {
	ts := testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	temporal.NewActivities(&nopLoader{}, rollout.NewRegistry()).Register(env)
	return env
}

// nopLoader is a Loader stub: the mocked activities short-circuit
// before calling LoadCluster, but Activities still needs a non-nil
// dependency at registration time.
type nopLoader struct{}

func (n *nopLoader) LoadCluster(_ context.Context, _ uuid.UUID) (rollout.Cluster, error) {
	panic("mocked activities must short-circuit before LoadCluster")
}

// TestTemporalExecute_ClusterGuard_StateRoundTrip locks in the core
// envelope contract for the temporal executor: a Payload returned by
// the ClusterGuard.PreRollout activity is delivered byte-identically
// to the matching PostRollout activity. The test mocks both activities
// and asserts the State.Payload received by PostRollout matches what
// PreRollout returned.
func TestTemporalExecute_ClusterGuard_StateRoundTrip(t *testing.T) {
	t.Parallel()

	const silenceID = "silence-uuid-7"

	env := newGuardTestEnv()

	// Stub the PreRollout activity to return a Payload containing
	// the silence ID we expect to round-trip.
	env.OnActivity(temporal.ActivityClusterGuardPreRollout, mock.Anything, mock.Anything).
		Return(&temporal.ClusterGuardPreRolloutResponse{
			State: rollout.GuardState{GuardName: "silence", Payload: []byte(silenceID)},
		}, nil).Once()

	// Capture the State the PostRollout activity is dispatched with
	// via mock.Call.Run, which fires exactly once on a matched call.
	var postState rollout.GuardState
	env.OnActivity(temporal.ActivityClusterGuardPostRollout, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.ClusterGuardPostRolloutRequest)
			postState = req.State
		}).Return(nil).Once()

	cluster := &fakeCluster{id: uuid.New(), cps: nil} // empty node set: PreRollout -> PostRollout, no apply loop

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			ClusterGuards: []rollout.ClusterGuardRef{{Name: "silence"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultCompleted, result.Status)

	// The point of the test: PostRollout must have seen exactly the
	// envelope PreRollout returned.
	assert.Equal(t, "silence", postState.GuardName)
	assert.Equal(t, silenceID, string(postState.Payload),
		"PostRollout must receive the same Payload bytes PreRollout returned")
}

// TestTemporalExecute_ClusterGuard_PartialStateFailure_PostRolloutStillFires
// locks in the contract that a ClusterGuard.PreRollout reporting a
// business failure ALONGSIDE a non-empty state Payload still gets its
// matching PostRollout dispatched. This is the partial-state cleanup
// path: a guard that took a real-world side effect (created a silence,
// opened a window) before failing must have its PostRollout fire so it
// can clean up.
//
// Under the previous (resp-with-payload, err) shape this path was
// silently broken in production because the Temporal Go SDK strips
// response values on non-nil activity error (sdk-go#1201) — meaning
// the workflow never saw the State.Payload that should have triggered
// the unwind. The new (resp{State, PreRolloutErr}, nil) shape carries
// the response across the SDK boundary intact regardless of business
// outcome. This test is the production-path regression guard.
func TestTemporalExecute_ClusterGuard_PartialStateFailure_PostRolloutStillFires(t *testing.T) {
	t.Parallel()

	const silenceID = "silence-uuid-partial-7"

	env := newGuardTestEnv()

	// PreRollout reports a business failure AND returns a populated
	// State envelope — the partial-state path. The activity itself
	// succeeds at the SDK boundary (returns (resp, nil)).
	env.OnActivity(temporal.ActivityClusterGuardPreRollout, mock.Anything, mock.Anything).
		Return(&temporal.ClusterGuardPreRolloutResponse{
			State:         rollout.GuardState{GuardName: "silence", Payload: []byte(silenceID)},
			PreRolloutErr: `cluster guard "silence" PreRollout: backend transiently unavailable`,
		}, nil).Once()

	// Verify PostRollout fires for cleanup, with the same Payload.
	var postState rollout.GuardState
	postRolloutCalls := 0
	env.OnActivity(temporal.ActivityClusterGuardPostRollout, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.ClusterGuardPostRolloutRequest)
			postState = req.State
			postRolloutCalls++
		}).Return(nil).Once()

	cluster := &fakeCluster{id: uuid.New(), cps: nil}

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			ClusterGuards: []rollout.ClusterGuardRef{{Name: "silence"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"business failure must surface as outcome-class, not workflow error")

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	require.NotNil(t, result.PhaseFailure, "cluster-guard PreRollout failure must surface via PhaseFailure")
	assert.Equal(t, rollout.PhaseClusterGuard, result.PhaseFailure.Phase)
	assert.Equal(t, "silence", result.PhaseFailure.Name)
	assert.Contains(t, result.PhaseFailure.Err, "backend transiently unavailable")

	// THE LOAD-BEARING ASSERTION: PostRollout fired with the same
	// Payload PreRollout produced. If the workflow consumer regressed
	// to ignoring resp.State on the failure path, postRolloutCalls
	// would be 0 and the side-effect would leak in production.
	assert.Equal(t, 1, postRolloutCalls,
		"PostRollout must fire for a guard whose PreRollout reported partial-state failure")
	assert.Equal(t, "silence", postState.GuardName)
	assert.Equal(t, silenceID, string(postState.Payload),
		"PostRollout must receive the same Payload bytes PreRollout reported on the partial-state path")
}

// TestTemporalExecute_NodeGuard_PartialStateFailure_PostApplyStillFires
// is the per-node sibling of the cluster-scope partial-state test.
// Same contract: a NodeGuard.PreApply that reports a business failure
// with a non-empty State Payload still gets its PostApply dispatched.
func TestTemporalExecute_NodeGuard_PartialStateFailure_PostApplyStillFires(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nodeID, name: "cp-0"}}}

	const windowToken = "drain-token-partial-9"

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State:       rollout.GuardState{GuardName: "drain", Payload: []byte(windowToken)},
			PreApplyErr: `node guard "drain" PreApply: kube-vip API timeout after partial drain`,
		}, nil).Once()

	// ApplyToNode must NOT be called — the guard failed.
	applyCallCount := 0
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { applyCallCount++ }).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeUnchanged}, nil).
		Maybe()

	// PostApply MUST fire with the partial-state Payload.
	var postState rollout.GuardState
	postApplyCalls := 0
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardPostApplyRequest)
			postState = req.State
			postApplyCalls++
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, nodeID, result.Failed()[0].NodeID)
	assert.Contains(t, result.Failed()[0].Err, "kube-vip API timeout after partial drain")

	assert.Zero(t, applyCallCount, "ApplyToNode must not run when NodeGuard.PreApply fails")
	assert.Equal(t, 1, postApplyCalls,
		"PostApply must fire for a guard whose PreApply reported partial-state failure")
	assert.Equal(t, "drain", postState.GuardName)
	assert.Equal(t, windowToken, string(postState.Payload),
		"PostApply must receive the same Payload bytes PreApply reported on the partial-state path")
}

// TestTemporalExecute_NodeGuard_StateRoundTrip locks in the
// per-node envelope contract: PreApply state flows to the matching
// PostApply activity. PostApply runs in reverse PreApply order and
// always fires after ApplyToNode.
func TestTemporalExecute_NodeGuard_StateRoundTrip(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nodeID, name: "cp-0"}}}

	const windowToken = "window-token-42"

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State: rollout.GuardState{GuardName: "drain", Payload: []byte(windowToken)},
		}, nil).Once()

	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeUnchanged}, nil).Once()

	var postState rollout.GuardState
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardPostApplyRequest)
			postState = req.State
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultCompleted, result.Status)

	assert.Equal(t, "drain", postState.GuardName)
	assert.Equal(t, windowToken, string(postState.Payload),
		"NodeGuard.PostApply must receive the same Payload bytes PreApply returned")
}

func TestTemporalExecute_NodeGuard_CheckersPollFromWorkflow(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nodeID, name: "cp-0"}}}

	const windowToken = "window-token-async-check"

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State:                  rollout.GuardState{GuardName: "drain", Payload: []byte(windowToken)},
			CheckPreApplyRequired:  true,
			CheckPostApplyRequired: true,
		}, nil).Once()

	preCheckCount := 0
	env.OnActivity(temporal.ActivityNodeGuardCheckPreApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardCheckPreApplyRequest)
			assert.Equal(t, "drain", req.State.GuardName)
			assert.Equal(t, windowToken, string(req.State.Payload))
			preCheckCount++
		}).
		Return(&temporal.NodeGuardCheckPreApplyResponse{Done: false}, nil).Once()
	env.OnActivity(temporal.ActivityNodeGuardCheckPreApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardCheckPreApplyRequest)
			assert.Equal(t, "drain", req.State.GuardName)
			assert.Equal(t, windowToken, string(req.State.Payload))
			preCheckCount++
		}).
		Return(&temporal.NodeGuardCheckPreApplyResponse{Done: true}, nil).Once()

	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()

	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(nil).Once()

	postCheckCount := 0
	env.OnActivity(temporal.ActivityNodeGuardCheckPostApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardCheckPostApplyRequest)
			assert.Equal(t, "drain", req.State.GuardName)
			assert.Equal(t, windowToken, string(req.State.Payload))
			assert.Equal(t, rollout.OutcomeApplied, req.Outcome)
			postCheckCount++
		}).
		Return(&temporal.NodeGuardCheckPostApplyResponse{Done: false}, nil).Once()
	env.OnActivity(temporal.ActivityNodeGuardCheckPostApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardCheckPostApplyRequest)
			assert.Equal(t, "drain", req.State.GuardName)
			assert.Equal(t, windowToken, string(req.State.Payload))
			assert.Equal(t, rollout.OutcomeApplied, req.Outcome)
			postCheckCount++
		}).
		Return(&temporal.NodeGuardCheckPostApplyResponse{Done: true}, nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultCompleted, result.Status)
	assert.Equal(t, 2, preCheckCount)
	assert.Equal(t, 2, postCheckCount)
}

func TestTemporalExecute_NodeGuard_PostApplyFailureStillRunsPostCheck(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nodeID, name: "cp-0"}}}

	const windowToken = "window-token-postapply-failure"

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State:                  rollout.GuardState{GuardName: "drain", Payload: []byte(windowToken)},
			CheckPostApplyRequired: true,
		}, nil).Once()

	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()

	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(errors.New("post apply failed")).Once()

	postCheckCount := 0
	env.OnActivity(temporal.ActivityNodeGuardCheckPostApply, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.NodeGuardCheckPostApplyRequest)
			assert.Equal(t, "drain", req.State.GuardName)
			assert.Equal(t, windowToken, string(req.State.Payload))
			assert.Equal(t, rollout.OutcomeApplied, req.Outcome)
			postCheckCount++
		}).
		Return(&temporal.NodeGuardCheckPostApplyResponse{CheckErr: "post check failed"}, nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome)
	assert.Contains(t, result.Failed()[0].Err, "post apply failed")
	assert.Contains(t, result.Failed()[0].Err, "post check failed")
	assert.Equal(t, 1, postCheckCount)
}

// ============================================================================
// RollbackPolicy + MinHealthyReplicas behavior
// ============================================================================
//
// Pin the executor's handling of spec.RollbackPolicy (HaltOnAnyFailure
// cascades on FailedRestored) and spec.MinHealthyReplicas (look-ahead aborts
// when remaining + healthy nodes can no longer satisfy MinHealthy).

// TestTemporalExecute_HaltOnAnyFailure_CascadesEvenOnFailedRestored
// drives a 3-CP rollout with RollbackPolicy=HaltOnAnyFailure where
// cp-2's ApplyToNode returns OutcomeFailedRestored. With the default
// CascadeOnUnrecoverable policy this would halt (Status=Failed) without
// cascading. Under HaltOnAnyFailure the executor MUST cascade-rollback
// cp-0 and cp-1 in reverse order.
func TestTemporalExecute_HaltOnAnyFailure_CascadesEvenOnFailedRestored(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	node1ID := uuid.New()
	node2ID := uuid.New()
	cluster := &fakeCluster{
		id: uuid.New(),
		cps: []rollout.Node{
			fakeNode{id: node0ID, name: "cp-0"},
			fakeNode{id: node1ID, name: "cp-1"},
			fakeNode{id: node2ID, name: "cp-2"},
		},
	}

	env := newGuardTestEnv()

	// cp-0 + cp-1 Applied, cp-2 FailedRestored.
	applyCount := 0
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { applyCount++ }).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Twice()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { applyCount++ }).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeFailedRestored, ApplyErr: "cp-2 self-restored"}, nil).Once()

	// Rollback must walk cp-1 then cp-0 (reverse apply order).
	rollbackOrder := []uuid.UUID{}
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.RollbackRequest)
			rollbackOrder = append(rollbackOrder, req.NodeID)
		}).Return(nil).Twice()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&rollbackTestOp{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			RollbackPolicy: rollout.RollbackHaltOnAnyFailure,
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultRolledBack, result.Status,
		"HaltOnAnyFailure must cascade even on FailedRestored")
	assert.Equal(t, 3, applyCount, "every CP must be visited")
	require.Equal(t, []uuid.UUID{node1ID, node0ID}, rollbackOrder,
		"cascade must walk applied set in REVERSE order")
}

// TestTemporalExecute_InfeasibleMinHealthyReplicas_FailsAtEntry_NoSideEffects pins
// the entry-time feasibility check. A spec whose MinHealthy exceeds
// the live CP count is rejected before any activity dispatches — no
// ApplyToNode, no RollbackNode, no guard.
func TestTemporalExecute_InfeasibleMinHealthyReplicas_FailsAtEntry_NoSideEffects(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{
		id: uuid.New(),
		cps: []rollout.Node{
			fakeNode{id: uuid.New(), name: "cp-0"},
			fakeNode{id: uuid.New(), name: "cp-1"},
		},
	}

	env := newGuardTestEnv()
	// Deliberately set NO mock expectations on apply / rollback / guard
	// activities; testify-mock will fail the test if any unexpected
	// activity fires.

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&rollbackTestOp{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: 3},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status,
		"infeasible minHealthyReplicas rejected at entry; nothing was applied so ResultFailed (not RolledBack)")
	require.NotNil(t, result.PhaseFailure)
	assert.Equal(t, rollout.PhaseMinHealthyReplicas, result.PhaseFailure.Phase)
	assert.Contains(t, result.PhaseFailure.Err, "infeasible minHealthyReplicas")
	assert.Contains(t, result.PhaseFailure.Err, "MinHealthy=3")
	assert.Contains(t, result.PhaseFailure.Err, "control-plane count 2")
}

// TestTemporalExecute_OutcomeUnspecified_FailsClosedAndCascades pins
// the safety property of the OutcomeUnspecified=0 sentinel: a workflow
// that receives Outcome=0 in an ApplyToNodeResponse (whether because an
// activity returned the zero value or because the response was
// truncated) must trigger cascade, not silently treat the node as
// Applied.
func TestTemporalExecute_OutcomeUnspecified_FailsClosedAndCascades(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	node1ID := uuid.New()
	cluster := &fakeCluster{
		id: uuid.New(),
		cps: []rollout.Node{
			fakeNode{id: node0ID, name: "cp-0"},
			fakeNode{id: node1ID, name: "cp-1"},
		},
	}

	env := newGuardTestEnv()

	// cp-0 Applied; cp-1 returns the zero-value Outcome (Unspecified).
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeUnspecified}, nil).Once()

	// Cascade rolls back cp-0 only.
	rollbackOrder := []uuid.UUID{}
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.RollbackRequest)
			rollbackOrder = append(rollbackOrder, req.NodeID)
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&rollbackTestOp{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultRolledBack, result.Status,
		"Unspecified outcome must trigger cascade")
	require.Equal(t, []uuid.UUID{node0ID}, rollbackOrder,
		"cp-0 (the only applied node) must be rolled back")
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome)
	assert.Contains(t, result.Failed()[0].Err, "unexpected outcome Unspecified")
}

func TestTemporalExecute_RollbackFailureLeavesResultFailed(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	cluster := &fakeCluster{
		id: uuid.New(),
		cps: []rollout.Node{
			fakeNode{id: node0ID, name: "cp-0"},
			fakeNode{id: uuid.New(), name: "cp-1"},
		},
	}
	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{
			Outcome:  rollout.OutcomeFailedNotRestored,
			ApplyErr: "cp-1 apply failed",
		}, nil).Once()
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Return(errors.New("rollback cp-0 failed")).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(
			activityCtx(ctx),
			cluster,
			rollout.Compile(&rollbackTestOp{}),
			rollout.RolloutSpec{},
		)
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	require.Len(t, result.Failed(), 2)
	assert.Equal(t, node0ID, result.Failed()[1].NodeID)
	assert.Contains(t, result.Failed()[1].Err, "rollback")
}

// rollbackTestOp is a minimal Operation that advertises a RollbackHandler
// so the temporal executor dispatches ActivityRollbackNode during the
// cascade. Activities are mocked; the body is never called.
type rollbackTestOp struct{}

func (n *rollbackTestOp) Name() string { return "rollback-test-op" }
func (n *rollbackTestOp) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

func (n *rollbackTestOp) Rollback(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) error {
	return nil
}

// TestTemporalExecute_FirstNodeFailedNotRestored_NoRollbackHandler
// pins: when there is no RollbackHandler, ResultRolledBack is a lie.
// Status must be ResultFailed.
func TestTemporalExecute_FirstNodeFailedNotRestored_NoRollbackHandler(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: uuid.New(), name: "cp-0"}}}

	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeFailedNotRestored}, nil).Once()
	// No ActivityRollbackNode expectation — capability detection on
	// noRollbackOp yields HasRollbackHandler=false, so cascadeRollback
	// must short-circuit without dispatching anything.

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noRollbackOp{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status,
		"no RollbackHandler means cascadeRollback is a no-op; Status must reflect that")
}

// noRollbackOp is the minimum-required Operation (no Rollback method),
// so capability detection in Compile yields HasRollbackHandler=false.
type noRollbackOp struct{}

func (n *noRollbackOp) Name() string { return "no-rollback-op" }
func (n *noRollbackOp) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

// TestTemporalExecute_PreApplyUnwindFails_EscalatesAndCascades
// pins guard PreApply-unwind escalation.
//
// Scenario: cp-0 applies cleanly; cp-1's NodeGuard.PreApply fails AND
// returns a non-empty Payload (so the framework runs PostApply for
// partial-state cleanup); that PostApply also fails (the cleanup
// couldn't reverse the guard's side effect).
//
// The fix the test locks in: when the unwind itself errors, the node's
// outcome must escalate from FailedRestored to FailedNotRestored — a
// guard cleanup we couldn't complete may have left state on disk, so
// the node is operationally indistinguishable from one we failed to
// restore. With the failing node escalated, the executor must cascade
// the previously-applied nodes (cp-0) so the cluster returns to a
// known starting state. Pre-fix the Temporal executor silently
// discarded the unwind error, hardcoded FailedRestored, and halted
// without cascade — leaving cp-0 applied and the cp-1 service window
// unreported.
func TestTemporalExecute_PreApplyUnwindFails_EscalatesAndCascades(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	node1ID := uuid.New()
	cluster := &fakeCluster{
		id:  uuid.New(),
		cps: []rollout.Node{fakeNode{id: node0ID, name: "cp-0"}, fakeNode{id: node1ID, name: "cp-1"}},
	}

	env := newGuardTestEnv()

	// PreApply: cp-0 succeeds with empty state; cp-1 fails with a
	// non-empty payload so the framework runs PostApply for
	// partial-state cleanup.
	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State: rollout.GuardState{GuardName: "drain"},
		}, nil).Once()
	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State:       rollout.GuardState{GuardName: "drain", Payload: []byte("opened-state")},
			PreApplyErr: `node guard "drain" PreApply: forced fail with partial state`,
		}, nil).Once()

	// ApplyToNode: only cp-0 reaches apply (cp-1 skipped due to
	// PreApply failure).
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()

	// PostApply: cp-0 success; cp-1 unwind FAILS — this is the
	// load-bearing condition that triggers the escalation.
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(nil).Once()
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(errors.New("PostApply cleanup also failed")).Once()

	// Cascade must run cp-0's Rollback.
	rollbackOrder := []uuid.UUID{}
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.RollbackRequest)
			rollbackOrder = append(rollbackOrder, req.NodeID)
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&rollbackTestOp{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))

	assert.Equal(t, rollout.ResultRolledBack, result.Status,
		"unwind-failure escalation must trigger the cascade and surface as RolledBack")
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, node1ID, result.Failed()[0].NodeID)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome,
		"PreApply unwind failure must escalate the failing node's outcome to FailedNotRestored")
	// Both the original PreApply error and the unwind error must
	// appear in the per-node error string (errors.Join joins by
	// newline).
	errMsg := result.Failed()[0].Err
	assert.Contains(t, errMsg, "forced fail with partial state",
		"node error must surface the original PreApply failure")
	assert.Contains(t, errMsg, "PostApply cleanup also failed",
		"node error must surface the unwind failure")
	// cp-0 was rolled back as part of the cascade.
	require.Len(t, rollbackOrder, 1, "cascade rollback must run cp-0's Rollback")
	assert.Equal(t, node0ID, rollbackOrder[0])
}

// ============================================================================
// Behavioral coverage
// ============================================================================
//
// Pattern: inline workflow closure calls temporal.Execute; activities are
// mocked with OnActivity. Run() captures per-call arguments for ordering
// or error-string assertions.

// TestTemporalExecute_HappyPath_AllApplied locks in the baseline: a
// 3-CP rollout where every node returns OutcomeApplied ends with
// ResultCompleted and all three nodes in Applied().
func TestTemporalExecute_HappyPath_AllApplied(t *testing.T) {
	t.Parallel()

	node0, node1, node2 := uuid.New(), uuid.New(), uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: node0, name: "cp-0"},
		fakeNode{id: node1, name: "cp-1"},
		fakeNode{id: node2, name: "cp-2"},
	}}

	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Times(3)

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultCompleted, result.Status)
	assert.Len(t, result.Applied(), 3)
	assert.Empty(t, result.Unchanged())
	assert.Empty(t, result.Failed())
	assert.NoError(t, result.Err())
}

// TestTemporalExecute_HappyPath_AllUnchanged locks in the Unchanged
// bucket: a rollout where every node returns OutcomeUnchanged is still
// ResultCompleted, with nodes in Unchanged() rather than Applied().
func TestTemporalExecute_HappyPath_AllUnchanged(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: uuid.New(), name: "cp-0"},
		fakeNode{id: uuid.New(), name: "cp-1"},
	}}

	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeUnchanged}, nil).Times(2)

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultCompleted, result.Status)
	assert.Empty(t, result.Applied())
	assert.Len(t, result.Unchanged(), 2)
	assert.Empty(t, result.Failed())
}

// TestTemporalExecute_FailedRestored_HaltsWithoutCascade locks in the
// default-policy halt semantics: cp-0 Applied, cp-1 FailedRestored with
// RollbackCascadeOnUnrecoverable (the default) → ResultFailed, no
// ActivityRollbackNode dispatched. FailedRestored self-recovered; the
// executor must NOT cascade to the already-applied cp-0.
func TestTemporalExecute_FailedRestored_HaltsWithoutCascade(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	node1ID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: node0ID, name: "cp-0"},
		fakeNode{id: node1ID, name: "cp-1"},
	}}

	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeFailedRestored, ApplyErr: "apiserver did not start"}, nil).Once()
	// No ActivityRollbackNode expectation: an unexpected call fails the test.

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&rollbackTestOp{}), rollout.RolloutSpec{
			RollbackPolicy: rollout.RollbackCascadeOnUnrecoverable,
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status,
		"FailedRestored with default policy halts without cascade; Status must be Failed, not RolledBack")
	assert.Len(t, result.Applied(), 1)
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, node1ID, result.Failed()[0].NodeID)
	assert.Equal(t, rollout.OutcomeFailedRestored, result.Failed()[0].Outcome)
}

// TestTemporalExecute_PostConditionFailure_ReportsFailedWithoutRollback
// locks in the post-condition phase: after all nodes apply successfully,
// a failing PostConditionChecker surfaces as ResultFailed with
// PhaseFailure{Phase: PhasePostCondition} and must NOT trigger cascade
// rollback (the nodes are in the desired state; rollback would regress them).
func TestTemporalExecute_PostConditionFailure_ReportsFailedWithoutRollback(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: uuid.New(), name: "cp-0"},
		fakeNode{id: uuid.New(), name: "cp-1"},
	}}

	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Times(2)
	env.OnActivity(temporal.ActivityCheckPostCond, mock.Anything, mock.Anything).
		Return(errors.New("apiserver not ready after rollout")).Once()
	// No ActivityRollbackNode expectation — post-condition failure must NOT cascade.

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&postCondCore{}), rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	assert.Len(t, result.Applied(), 2, "applied nodes must stay in Applied() — no cascade on post-condition failure")
	assert.Empty(t, result.Failed(), "post-condition failure must not synthesize Failed() entries")
	require.NotNil(t, result.PhaseFailure)
	assert.Equal(t, rollout.PhasePostCondition, result.PhaseFailure.Phase)
	assert.Contains(t, result.PhaseFailure.Err, "apiserver not ready after rollout")
}

// postCondCore is a minimal Operation + PostConditionChecker so that
// HasPostConditionChecker returns true on the wrapped Operation. The
// CheckPostCondition body is never reached — the activity is mocked.
type postCondCore struct{}

func (p *postCondCore) Name() string { return "post-cond-test" }
func (p *postCondCore) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeApplied, nil
}

func (p *postCondCore) CheckPostCondition(_ context.Context, _ rollout.Cluster) error { return nil }

// TestTemporalExecute_StatelessGuardPreApplyErrors_PostApplyStillFires
// pins the symmetric-cleanup contract for stateless guards (no
// Payload): a NodeGuard whose PreApply reports a business failure via
// PreApplyErr with an empty-payload State must still have its PostApply
// activity dispatched. Without this, a stateless-but-side-effecting guard
// (one that opens a service window and reports success-or-failure without
// emitting a Payload) could silently skip cleanup on failure.
func TestTemporalExecute_StatelessGuardPreApplyErrors_PostApplyStillFires(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: uuid.New(), name: "cp-0"},
	}}

	env := newGuardTestEnv()

	// PreApply: stateless business failure — no Payload, just PreApplyErr.
	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State:       rollout.GuardState{GuardName: "panoptes"},
			PreApplyErr: `node guard "panoptes" PreApply: silence backend transiently unavailable`,
		}, nil).Once()

	// PostApply MUST fire for the unwind, even with a nil-payload state.
	postApplyCalled := false
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { postApplyCalled = true }).
		Return(nil).Once()

	// ApplyToNode must NOT be called — guard PreApply failed.
	applyCallCount := 0
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { applyCallCount++ }).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Maybe()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "panoptes"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)
	assert.Zero(t, applyCallCount, "ApplyToNode must not run when NodeGuard.PreApply fails")
	assert.True(t, postApplyCalled,
		"PostApply MUST fire for a guard whose PreApply errored — even with no payload (symmetric cleanup contract)")
}

// TestTemporalExecute_TwoNodeGuards_PostApplyBothFail_AggregatesErrors
// locks in the multi-guard error aggregation contract: when two NodeGuards'
// PostApply activities both fail on the same Applied node, both error
// strings must appear in the per-node NodeResult.Err (via errors.Join),
// and the node's Outcome must escalate to FailedNotRestored.
func TestTemporalExecute_TwoNodeGuards_PostApplyBothFail_AggregatesErrors(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: nodeID, name: "cp-0"},
	}}

	env := newGuardTestEnv()

	// Both guards PreApply successfully.
	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State: rollout.GuardState{GuardName: "guard-a"},
		}, nil).Once()
	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State: rollout.GuardState{GuardName: "guard-b"},
		}, nil).Once()

	// Apply succeeds.
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()

	// PostApply runs in reverse order (guard-b then guard-a); both fail.
	// runNodeGuardPostApply walks the slice in reverse, so guard-b's
	// PostApply dispatches first.
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(errors.New("close-b failed")).Once()
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(errors.New("close-a failed")).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{
				{Name: "guard-a"},
				{Name: "guard-b"},
			},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Len(t, result.Failed(), 1,
		"PostApply errors on an Applied node escalate to FailedNotRestored")
	assert.Equal(t, nodeID, result.Failed()[0].NodeID)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome,
		"PostApply failure on Applied node must escalate to FailedNotRestored")

	// Both guard errors must appear in the joined per-node error string.
	errMsg := result.Failed()[0].Err
	assert.Contains(t, errMsg, "close-a failed",
		"guard-a's PostApply failure must appear in per-node error")
	assert.Contains(t, errMsg, "close-b failed",
		"guard-b's PostApply failure must appear in per-node error")
}

// TestTemporalExecute_ApplyActivityErrAndPostApplyErr_AggregatesAndCascades
// locks in the contract that when ApplyToNode's activity itself fails
// (a dispatch error, distinct from a business-error Outcome) AND the
// subsequent NodeGuard.PostApply also fails, both errors are joined
// into the per-node NodeResult.Err. The node's Outcome is
// FailedNotRestored either way; the regression this guards against is
// the PostApply error being silently dropped while the apply-dispatch
// error alone surfaces.
func TestTemporalExecute_ApplyActivityErrAndPostApplyErr_AggregatesAndCascades(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	node1ID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: node0ID, name: "cp-0"},
		fakeNode{id: node1ID, name: "cp-1"},
	}}

	env := newGuardTestEnv()

	// PreApply: both succeed.
	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State: rollout.GuardState{GuardName: "drain"},
		}, nil).Twice()

	// cp-0 applies cleanly; cp-1's ApplyToNode activity itself fails
	// (dispatch error, not a business-error Outcome).
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(nil, errors.New("apply dispatch failed")).Once()

	// PostApply: cp-0 closes cleanly; cp-1's unwind also fails.
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(nil).Once()
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(errors.New("postapply drain unwind failed")).Once()

	// cp-0 must be rolled back as part of the cascade.
	rollbackOrder := []uuid.UUID{}
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.RollbackRequest)
			rollbackOrder = append(rollbackOrder, req.NodeID)
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&rollbackTestOp{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))

	assert.Equal(t, rollout.ResultRolledBack, result.Status,
		"apply-dispatch failure on a touched node must cascade the cp-0 rollback")
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, node1ID, result.Failed()[0].NodeID)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome,
		"apply-dispatch failure stamps the node as FailedNotRestored")

	errMsg := result.Failed()[0].Err
	assert.Contains(t, errMsg, "apply dispatch failed",
		"per-node error must surface the apply activity failure")
	assert.Contains(t, errMsg, "postapply drain unwind failed",
		"per-node error must also surface the PostApply unwind failure (regression guard: postErr must not be dropped when applyErr is already set)")

	require.Len(t, rollbackOrder, 1, "cascade rollback must run cp-0's Rollback")
	assert.Equal(t, node0ID, rollbackOrder[0])
}

// TestTemporalExecute_ClusterGuard_PostRolloutFiresOnFailedRollout locks
// in the always-fire invariant for ClusterGuard.PostRollout: when the
// rollout fails (first node FailedNotRestored, nothing to cascade) the
// PostRollout activity must still dispatch with the failing Result so the
// guard can close any service window it opened.
func TestTemporalExecute_ClusterGuard_PostRolloutFiresOnFailedRollout(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: uuid.New(), name: "cp-0"},
	}}

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityClusterGuardPreRollout, mock.Anything, mock.Anything).
		Return(&temporal.ClusterGuardPreRolloutResponse{
			State: rollout.GuardState{GuardName: "silence"},
		}, nil).Once()

	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeFailedNotRestored}, nil).Once()

	// PostRollout must fire; capture the Result it receives.
	var postResult rollout.Result
	env.OnActivity(temporal.ActivityClusterGuardPostRollout, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.ClusterGuardPostRolloutRequest)
			postResult = req.Result
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{
			ClusterGuards: []rollout.ClusterGuardRef{{Name: "silence"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status,
		"first node FailedNotRestored with no prior applied nodes: nothing to roll back, Status=Failed")
	assert.Equal(t, rollout.ResultFailed, postResult.Status,
		"PostRollout must receive the failing Result so the guard can see why the rollout ended")
}

// TestTemporalExecute_UnsupportedStrategy_ReturnsExecutorError locks in
// the executor-internal (infrastructure-class) error path for an
// unsupported RolloutSpec strategy. Distinct from outcome-class failures:
// the workflow itself returns a non-nil error (GetWorkflowError != nil).
func TestTemporalExecute_UnsupportedStrategy_ReturnsExecutorError(t *testing.T) {
	t.Parallel()

	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: uuid.New(), name: "cp-0"},
	}}

	env := newGuardTestEnv()
	// No activity mocks: the executor must reject before any dispatch.

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{
			Strategy: "Parallel",
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(),
		"unsupported strategy is an executor-internal error, not an outcome-class failure")
}

// TestTemporalExecute_OutOfRangeOutcome_FailsClosedAndCascades extends the
// Unspecified-outcome test to an arbitrary unknown value (99). Both routes
// through the default case in the outcome switch: any numeric value the
// executor doesn't recognize must trigger cascade, not silently pass.
func TestTemporalExecute_OutOfRangeOutcome_FailsClosedAndCascades(t *testing.T) {
	t.Parallel()

	node0ID := uuid.New()
	node1ID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: node0ID, name: "cp-0"},
		fakeNode{id: node1ID, name: "cp-1"},
	}}

	env := newGuardTestEnv()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.OutcomeApplied}, nil).Once()
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{Outcome: rollout.Outcome(99)}, nil).Once()

	rollbackOrder := []uuid.UUID{}
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.RollbackRequest)
			rollbackOrder = append(rollbackOrder, req.NodeID)
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&rollbackTestOp{}), rollout.RolloutSpec{})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultRolledBack, result.Status,
		"out-of-range outcome must trigger cascade, same as Unspecified")
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome)
	assert.Contains(t, result.Failed()[0].Err, "unexpected outcome")
	require.Equal(t, []uuid.UUID{node0ID}, rollbackOrder,
		"cascade must roll back the only previously applied node")
}

// TestTemporalExecute_ClusterGuard_PostRolloutFiresOnAllExitPaths locks
// in that ClusterGuard.PostRollout dispatches on EVERY rollout exit path
// after PreRollout has opened. Specifically: a node-preflight failure
// mid-walk (after cp-0 applied cleanly) must still invoke PostRollout
// with a disconnected workflow context (workflow.NewDisconnectedContext)
// so that guards which opened service windows can close them regardless
// of how the rollout terminated.
//
// The disconnected-context guarantee means PostRollout dispatches even
// if the workflow receives a cancellation signal between PreRollout and
// the halt — workflow cancellation cannot abort guard cleanup.
func TestTemporalExecute_ClusterGuard_PostRolloutFiresOnAllExitPaths(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{
		fakeNode{id: nodeID, name: "cp-0"},
	}}

	env := newGuardTestEnv()

	// Guard opens in PreRollout.
	env.OnActivity(temporal.ActivityClusterGuardPreRollout, mock.Anything, mock.Anything).
		Return(&temporal.ClusterGuardPreRolloutResponse{
			State: rollout.GuardState{GuardName: "silence"},
		}, nil).Once()

	// NodePreflight fails — halts the rollout before any node is applied,
	// giving us a clean "halt after guard opened, nothing applied" path.
	env.OnActivity(temporal.ActivityNodePreflight, mock.Anything, mock.Anything).
		Return(errors.New("preflight refused")).Once()

	// THE LOAD-BEARING ASSERTION: PostRollout must fire on the
	// disconnected context even on this early-halt path.
	postRolloutDispatched := false
	var postResult rollout.Result
	env.OnActivity(temporal.ActivityClusterGuardPostRollout, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*temporal.ClusterGuardPostRolloutRequest)
			postRolloutDispatched = true
			postResult = req.Result
		}).Return(nil).Once()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		return temporal.Execute(activityCtx(ctx), cluster, rollout.Compile(&noopCore{}), rollout.RolloutSpec{
			ClusterGuards:  []rollout.ClusterGuardRef{{Name: "silence"}},
			NodePreflights: []rollout.NodePreflightRef{{Name: "block"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, rollout.ResultFailed, result.Status)

	assert.True(t, postRolloutDispatched,
		"ClusterGuardPostRollout MUST fire on every rollout exit path after PreRollout opened — "+
			"it dispatches on workflow.NewDisconnectedContext so it is not suppressed by workflow cancellation or halt")
	assert.Equal(t, rollout.ResultFailed, postResult.Status,
		"PostRollout receives the rollout Result so guards know why the rollout ended")
}

// TestTemporalExecute_ApplyErr_FoldedWithPostApplyFailure pins C-1: when
// ApplyToNode dispatched and the operation reported its own detail
// (resp.ApplyErr) and a PostApply guard then fails, the Failed() entry
// must surface BOTH the operation detail and the post-apply guard error —
// the operation detail must not be dropped in favor of the guard error.
func TestTemporalExecute_ApplyErr_FoldedWithPostApplyFailure(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nodeID, name: "cp-0"}}}

	env := newGuardTestEnv()

	env.OnActivity(temporal.ActivityNodeGuardPreApply, mock.Anything, mock.Anything).
		Return(&temporal.NodeGuardPreApplyResponse{
			State: rollout.GuardState{GuardName: "drain"},
		}, nil).Once()

	// ApplyToNode dispatches and the operation reports business detail.
	env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
		Return(&temporal.ApplyToNodeResponse{
			Outcome:  rollout.OutcomeFailedRestored,
			ApplyErr: "cp-0 self-restored: apiserver rejected --oidc flag",
		}, nil).Once()

	// PostApply then fails; its error joins applyErr.
	env.OnActivity(temporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).
		Return(errors.New("etcd health re-check failed after apply")).Once()

	// noopCore has no rollback handler; tolerate but don't require a probe.
	env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
		Return(nil).Maybe()

	wf := func(ctx workflow.Context) (rollout.Result, error) {
		op := rollout.Compile(&noopCore{})
		return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{
			NodeGuards: []rollout.NodeGuardRef{{Name: "drain"}},
		})
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result rollout.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Len(t, result.Failed(), 1)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, result.Failed()[0].Outcome)
	assert.Contains(t, result.Failed()[0].Err, "cp-0 self-restored: apiserver rejected --oidc flag",
		"C-1: operation detail (resp.ApplyErr) must not be dropped on the dispatch/cleanup-error path")
	assert.Contains(t, result.Failed()[0].Err, "etcd health re-check failed after apply",
		"C-1: the post-apply guard error must also be surfaced")
}

// TestTemporalExecute_ApplyErr_OnlyOnFailureRows pins L-3: resp.ApplyErr is
// attached to a NodeResult only for failure outcomes. A success row
// (Applied/Unchanged) must never carry an error string, even when a
// misbehaving operation populates ApplyErr on a success outcome.
func TestTemporalExecute_ApplyErr_OnlyOnFailureRows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		outcome  rollout.Outcome
		applyErr string
		bucket   func(rollout.Result) []rollout.NodeResult
		wantErr  string
	}{
		{
			name:     "Applied clears ApplyErr",
			outcome:  rollout.OutcomeApplied,
			applyErr: "spurious warning on a successful apply",
			bucket:   func(r rollout.Result) []rollout.NodeResult { return r.Applied() },
			wantErr:  "",
		},
		{
			name:     "Unchanged clears ApplyErr",
			outcome:  rollout.OutcomeUnchanged,
			applyErr: "spurious warning on a no-op",
			bucket:   func(r rollout.Result) []rollout.NodeResult { return r.Unchanged() },
			wantErr:  "",
		},
		{
			name:     "FailedRestored retains ApplyErr",
			outcome:  rollout.OutcomeFailedRestored,
			applyErr: "cp-0 self-restored: flag rejected",
			bucket:   func(r rollout.Result) []rollout.NodeResult { return r.Failed() },
			wantErr:  "cp-0 self-restored: flag rejected",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodeID := uuid.New()
			cluster := &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nodeID, name: "cp-0"}}}

			env := newGuardTestEnv()
			env.OnActivity(temporal.ActivityApplyToNode, mock.Anything, mock.Anything).
				Return(&temporal.ApplyToNodeResponse{Outcome: tc.outcome, ApplyErr: tc.applyErr}, nil).Once()
			env.OnActivity(temporal.ActivityRollbackNode, mock.Anything, mock.Anything).
				Return(nil).Maybe()

			wf := func(ctx workflow.Context) (rollout.Result, error) {
				op := rollout.Compile(&noopCore{})
				return temporal.Execute(activityCtx(ctx), cluster, op, rollout.RolloutSpec{})
			}
			env.RegisterWorkflow(wf)
			env.ExecuteWorkflow(wf)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())

			var result rollout.Result
			require.NoError(t, env.GetWorkflowResult(&result))

			bucket := tc.bucket(result)
			require.Len(t, bucket, 1)
			assert.Equal(t, nodeID, bucket[0].NodeID)
			assert.Equal(t, tc.wantErr, bucket[0].Err)
		})
	}
}
