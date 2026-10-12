// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal_test

// Direct activity-layer tests. executor_test.go mocks activities and
// asserts the workflow's reaction; this file asserts the producer
// side: the (resp, nil) sdk-go#1201 split, Post* GuardName-mismatch
// rejection, happy paths, and preflight resolution.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktemporal "go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
)

// fixedLoader returns a preset cluster (or error).
type fixedLoader struct {
	cluster rollout.Cluster
	err     error
}

func (l *fixedLoader) LoadCluster(_ context.Context, _ uuid.UUID) (rollout.Cluster, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.cluster, nil
}

// cfgCore is an Operation where each capability delegates to the
// supplied func. nil field = trivial success.
type cfgCore struct {
	name     string
	apply    func() (rollout.Outcome, error)
	isSat    func() (bool, error)
	build    func() (rollout.ApplyToNodePayload, error)
	validate func() error
	rollback func() error
	postCond func() error
}

func (c *cfgCore) Name() string { return c.name }

func (c *cfgCore) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	if c.apply != nil {
		return c.apply()
	}
	return rollout.OutcomeApplied, nil
}

func (c *cfgCore) IsSatisfied(_ context.Context, _ rollout.Cluster) (bool, error) {
	if c.isSat != nil {
		return c.isSat()
	}
	return false, nil
}

func (c *cfgCore) BuildApplyToNodePayload(_ context.Context, _ rollout.Cluster) (rollout.ApplyToNodePayload, error) {
	if c.build != nil {
		return c.build()
	}
	return rollout.ApplyToNodePayload("default-payload"), nil
}

func (c *cfgCore) Validate(_ context.Context, _ rollout.Cluster) error {
	if c.validate != nil {
		return c.validate()
	}
	return nil
}

func (c *cfgCore) Rollback(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) error {
	if c.rollback != nil {
		return c.rollback()
	}
	return nil
}

func (c *cfgCore) CheckPostCondition(_ context.Context, _ rollout.Cluster) error {
	if c.postCond != nil {
		return c.postCond()
	}
	return nil
}

// --- registry helpers (register into the test's fresh registry) ---

func registerNodeGuard(t *testing.T, reg *rollout.Registry, name string, f rollout.NodeGuardFactory) {
	t.Helper()
	reg.RegisterNodeGuard(name, f)
}

func registerClusterGuard(t *testing.T, reg *rollout.Registry, name string, f rollout.ClusterGuardFactory) {
	t.Helper()
	reg.RegisterClusterGuard(name, f)
}

func registerClusterPreflight(t *testing.T, reg *rollout.Registry, name string, f rollout.ClusterPreflightFunc) {
	t.Helper()
	reg.RegisterClusterPreflight(name, f)
}

func registerNodePreflight(t *testing.T, reg *rollout.Registry, name string, f rollout.NodePreflightFunc) {
	t.Helper()
	reg.RegisterNodePreflight(name, f)
}

func requireNonRetryable(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var appErr *sdktemporal.ApplicationError
	require.ErrorAs(t, err, &appErr, "expected *temporal.ApplicationError, got %T", err)
	assert.True(t, appErr.NonRetryable(), "cleanup failure must be non-retryable so the SDK does not re-dispatch it")
}

// requireRetryable asserts that err is a plain (non-Temporal-application)
// error and so will be retried by the SDK under the default retry
// policy. Used to lock in that transient infrastructure failures
// (cluster load) do NOT get wrapped as non-retryable inside cleanup
// activities — a postgres flake mid-cascade must not permanently fail
// the rollback step.
func requireRetryable(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var appErr *sdktemporal.ApplicationError
	if errors.As(err, &appErr) {
		assert.False(t, appErr.NonRetryable(),
			"load-failure path must stay retryable; got non-retryable %v", err)
	}
}

// clusterWithNode returns a fakeCluster holding one CP node named
// "cp-1" and that node's ID, for activities that resolve a node via
// findNode. The node name is set explicitly because the failing-guard
// fixtures key their injected failures on node name (e.g.
// ParamFailPreApplyOn: "cp-1").
func clusterWithNode() (*fakeCluster, uuid.UUID) {
	nid := uuid.New()
	return &fakeCluster{id: uuid.New(), cps: []rollout.Node{fakeNode{id: nid, name: "cp-1"}}}, nid
}

type asyncNodeGuard struct {
	name     string
	preDone  bool
	preErr   error
	postDone bool
	postErr  error
}

func (g *asyncNodeGuard) Name() string { return g.name }

func (g *asyncNodeGuard) PreApply(context.Context, rollout.Cluster, rollout.Node) (rollout.GuardState, error) {
	return rollout.StatelessGuardState(g.name), nil
}

func (g *asyncNodeGuard) CheckPreApply(context.Context, rollout.Cluster, rollout.Node, rollout.GuardState) (bool, error) {
	return g.preDone, g.preErr
}

func (g *asyncNodeGuard) PostApply(context.Context, rollout.Cluster, rollout.Node, rollout.Outcome, rollout.GuardState) error {
	return nil
}

func (g *asyncNodeGuard) CheckPostApply(context.Context, rollout.Cluster, rollout.Node, rollout.Outcome, rollout.GuardState) (bool, error) {
	return g.postDone, g.postErr
}

// ---------------------------------------------------------------------
// Operation activities: the all-response (business) vs infrastructure
// split. Business failure -> (resp with *Err, nil). Infra failure ->
// (nil, err) so Temporal retries.
// ---------------------------------------------------------------------

func TestActivities_IsSatisfied_AllResponseSplit(t *testing.T) {
	t.Run("business error packs into IsSatErr, no SDK error", func(t *testing.T) {
		core := &cfgCore{name: "act-issat-biz", isSat: func() (bool, error) {
			return false, errors.New("tenant apiserver unreachable")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)

		resp, err := a.IsSatisfied(context.Background(), &temporal.IsSatisfiedRequest{
			OperationName: "act-issat-biz", ClusterID: uuid.New(),
		})
		require.NoError(t, err, "business failure must not surface at the SDK boundary (sdk-go#1201 would strip the response)")
		require.NotNil(t, resp)
		assert.False(t, resp.Satisfied)
		assert.Contains(t, resp.IsSatErr, "tenant apiserver unreachable")
	})

	t.Run("satisfied=true round-trips with empty IsSatErr", func(t *testing.T) {
		core := &cfgCore{name: "act-issat-ok", isSat: func() (bool, error) { return true, nil }}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)

		resp, err := a.IsSatisfied(context.Background(), &temporal.IsSatisfiedRequest{
			OperationName: "act-issat-ok", ClusterID: uuid.New(),
		})
		require.NoError(t, err)
		assert.True(t, resp.Satisfied)
		assert.Empty(t, resp.IsSatErr)
	})

	t.Run("infrastructure failure (op not registered) returns (nil, err)", func(t *testing.T) {
		reg := rollout.NewRegistry()
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		resp, err := a.IsSatisfied(context.Background(), &temporal.IsSatisfiedRequest{
			OperationName: "act-issat-missing", ClusterID: uuid.New(),
		})
		require.Error(t, err, "an unregistered operation is infrastructure failure — must surface as an SDK error so Temporal can retry")
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "not registered")
	})

	t.Run("infrastructure failure (cluster load) returns (nil, err)", func(t *testing.T) {
		core := &cfgCore{name: "act-issat-loadfail"}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{err: errors.New("db down")}, reg)
		resp, err := a.IsSatisfied(context.Background(), &temporal.IsSatisfiedRequest{
			OperationName: "act-issat-loadfail", ClusterID: uuid.New(),
		})
		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "load cluster")
	})
}

func TestActivities_BuildApplyToNodePayload_AllResponseSplit(t *testing.T) {
	t.Run("business error packs into BuildErr, no SDK error", func(t *testing.T) {
		core := &cfgCore{name: "act-build-biz", build: func() (rollout.ApplyToNodePayload, error) {
			return nil, errors.New("SPIRE config not found")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)

		resp, err := a.BuildApplyToNodePayload(context.Background(), &temporal.BuildApplyToNodePayloadRequest{
			OperationName: "act-build-biz", ClusterID: uuid.New(),
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Contains(t, resp.BuildErr, "SPIRE config not found")
		assert.Empty(t, resp.Payload)
	})

	t.Run("business error discards a partial payload", func(t *testing.T) {
		core := &cfgCore{name: "act-build-partial", build: func() (rollout.ApplyToNodePayload, error) {
			return rollout.ApplyToNodePayload("partial-invalid-bytes"), errors.New("render failed")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)

		resp, err := a.BuildApplyToNodePayload(context.Background(), &temporal.BuildApplyToNodePayloadRequest{
			OperationName: "act-build-partial", ClusterID: uuid.New(),
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Contains(t, resp.BuildErr, "render failed")
		assert.Empty(t, resp.Payload)
	})

	t.Run("success returns the payload bytes", func(t *testing.T) {
		core := &cfgCore{name: "act-build-ok", build: func() (rollout.ApplyToNodePayload, error) {
			return rollout.ApplyToNodePayload("rendered-bytes"), nil
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)

		resp, err := a.BuildApplyToNodePayload(context.Background(), &temporal.BuildApplyToNodePayloadRequest{
			OperationName: "act-build-ok", ClusterID: uuid.New(),
		})
		require.NoError(t, err)
		assert.Empty(t, resp.BuildErr)
		assert.Equal(t, rollout.ApplyToNodePayload("rendered-bytes"), resp.Payload)
	})
}

func TestActivities_ApplyToNode_OutcomeAndErrorPassthrough(t *testing.T) {
	t.Run("apply error packs into ApplyErr and preserves the reported Outcome", func(t *testing.T) {
		core := &cfgCore{name: "act-apply-biz", apply: func() (rollout.Outcome, error) {
			return rollout.OutcomeFailedRestored, errors.New("kubeadm exited 1")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)

		resp, err := a.ApplyToNode(context.Background(), &temporal.ApplyToNodeRequest{
			OperationName: "act-apply-biz", ClusterID: cl.id, NodeID: nid,
		})
		require.NoError(t, err, "an apply error is the operation's own outcome signal, not an SDK-boundary failure")
		require.NotNil(t, resp)
		assert.Equal(t, rollout.OutcomeFailedRestored, resp.Outcome,
			"the operation's Outcome must survive the boundary — it drives cascade-vs-halt")
		assert.Contains(t, resp.ApplyErr, "kubeadm exited 1")
	})

	t.Run("infrastructure failure (node not in cluster) returns (nil, err)", func(t *testing.T) {
		core := &cfgCore{name: "act-apply-nonode"}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		cl, _ := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)

		resp, err := a.ApplyToNode(context.Background(), &temporal.ApplyToNodeRequest{
			OperationName: "act-apply-nonode", ClusterID: cl.id, NodeID: uuid.New(), // not in cluster
		})
		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "not found")
	})
}

// ---------------------------------------------------------------------
// Guard Pre* activities: the partial-state producer path. This is the
// lynchpin of the sdk-go#1201 work-around — a PreApply/PreRollout that
// fails WITH a payload must deliver both the error message AND the
// payload to the workflow so PostApply/PostRollout cleanup can run.
// ---------------------------------------------------------------------

func TestActivities_NodeGuardPreApply_PartialStateProducer(t *testing.T) {
	t.Run("business error WITH payload preserves both PreApplyErr and State.Payload", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerNodeGuard(t, reg, "act-ng-partial", NewFailingNodeGuardFactory("act-ng-partial"))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)

		resp, err := a.NodeGuardPreApply(context.Background(), &temporal.NodeGuardPreApplyRequest{
			GuardName: "act-ng-partial",
			Params: map[string]string{
				ParamFailPreApplyOn:      "cp-1", // matches the fake node name
				ParamPreApplyEmitPayload: "1",
			},
			ClusterID: cl.id, NodeID: nid,
		})
		require.NoError(t, err, "guard business failure must return (resp, nil), not (nil, err)")
		require.NotNil(t, resp)
		assert.NotEmpty(t, resp.PreApplyErr, "the PreApply error must be packed into PreApplyErr")
		assert.Equal(t, "act-ng-partial", resp.State.GuardName)
		assert.NotEmpty(t, resp.State.Payload, "the partial-state Payload must cross the activity boundary intact")
	})

	t.Run("success returns the stateless envelope, no PreApplyErr", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerNodeGuard(t, reg, "act-ng-ok", NewFailingNodeGuardFactory("act-ng-ok"))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)

		resp, err := a.NodeGuardPreApply(context.Background(), &temporal.NodeGuardPreApplyRequest{
			GuardName: "act-ng-ok", ClusterID: cl.id, NodeID: nid,
		})
		require.NoError(t, err)
		assert.Empty(t, resp.PreApplyErr)
		assert.Equal(t, "act-ng-ok", resp.State.GuardName)
	})

	t.Run("infrastructure failure (guard not registered) returns (nil, err)", func(t *testing.T) {
		reg := rollout.NewRegistry()
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		resp, err := a.NodeGuardPreApply(context.Background(), &temporal.NodeGuardPreApplyRequest{
			GuardName: "act-ng-missing", ClusterID: cl.id, NodeID: nid,
		})
		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "not registered")
	})
}

func TestActivities_NodeGuardAsyncCheckers(t *testing.T) {
	reg := rollout.NewRegistry()
	registerNodeGuard(t, reg, "act-ng-async", func(map[string]string) (rollout.NodeGuard, error) {
		return &asyncNodeGuard{name: "act-ng-async", preDone: false, postDone: true}, nil
	})
	cl, nid := clusterWithNode()
	a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)

	preResp, err := a.NodeGuardPreApply(context.Background(), &temporal.NodeGuardPreApplyRequest{
		GuardName: "act-ng-async",
		ClusterID: cl.id,
		NodeID:    nid,
	})
	require.NoError(t, err)
	require.NotNil(t, preResp)
	assert.True(t, preResp.CheckPreApplyRequired)
	assert.True(t, preResp.CheckPostApplyRequired)

	checkPreResp, err := a.NodeGuardCheckPreApply(context.Background(), &temporal.NodeGuardCheckPreApplyRequest{
		GuardName: "act-ng-async",
		ClusterID: cl.id,
		NodeID:    nid,
		State:     preResp.State,
	})
	require.NoError(t, err)
	require.NotNil(t, checkPreResp)
	assert.False(t, checkPreResp.Done)
	assert.Empty(t, checkPreResp.CheckErr)

	checkPostResp, err := a.NodeGuardCheckPostApply(context.Background(), &temporal.NodeGuardCheckPostApplyRequest{
		GuardName: "act-ng-async",
		ClusterID: cl.id,
		NodeID:    nid,
		Outcome:   rollout.OutcomeApplied,
		State:     preResp.State,
	})
	require.NoError(t, err)
	require.NotNil(t, checkPostResp)
	assert.True(t, checkPostResp.Done)
	assert.Empty(t, checkPostResp.CheckErr)
}

func TestActivities_ClusterGuardPreRollout_PartialStateProducer(t *testing.T) {
	t.Run("business error WITH payload preserves both PreRolloutErr and State.Payload", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerClusterGuard(t, reg, "act-cg-partial", NewFailingClusterGuardFactory("act-cg-partial"))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)

		resp, err := a.ClusterGuardPreRollout(context.Background(), &temporal.ClusterGuardPreRolloutRequest{
			GuardName: "act-cg-partial",
			Params: map[string]string{
				ParamFailPreRollout:    "1",
				ParamPreRolloutPayload: "1",
			},
			ClusterID: uuid.New(),
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.NotEmpty(t, resp.PreRolloutErr)
		assert.Equal(t, "act-cg-partial", resp.State.GuardName)
		assert.NotEmpty(t, resp.State.Payload, "the partial-state Payload must cross the activity boundary intact")
	})

	t.Run("infrastructure failure (guard not registered) returns (nil, err)", func(t *testing.T) {
		reg := rollout.NewRegistry()
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		resp, err := a.ClusterGuardPreRollout(context.Background(), &temporal.ClusterGuardPreRolloutRequest{
			GuardName: "act-cg-missing", ClusterID: uuid.New(),
		})
		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "not registered")
	})
}

// ---------------------------------------------------------------------
// Guard Post* activities: the GuardName-mismatch assertion (the empty
// case is covered in executor_test.go) plus the success path.
// ---------------------------------------------------------------------

func TestActivities_NodeGuardPostApply_GuardNameMismatch_NonRetryable(t *testing.T) {
	// The envelope belongs to "drain" but the request targets "silence":
	// a mis-routed per-guard map would otherwise hand the wrong payload to
	// the wrong guard. The assertion fires before any guard/cluster lookup,
	// so a nopLoader is sufficient.
	a := temporal.NewActivities(&nopLoader{}, rollout.NewRegistry())
	err := a.NodeGuardPostApply(context.Background(), &temporal.NodeGuardPostApplyRequest{
		GuardName: "silence",
		ClusterID: uuid.New(),
		NodeID:    uuid.New(),
		Outcome:   rollout.OutcomeApplied,
		State:     rollout.GuardState{GuardName: "drain", Payload: []byte("window-token")},
	})
	requireNonRetryable(t, err)
	assert.Contains(t, err.Error(), "drain")
	assert.Contains(t, err.Error(), "silence")
}

func TestActivities_ClusterGuardPostRollout_GuardNameMismatch_NonRetryable(t *testing.T) {
	a := temporal.NewActivities(&nopLoader{}, rollout.NewRegistry())
	err := a.ClusterGuardPostRollout(context.Background(), &temporal.ClusterGuardPostRolloutRequest{
		GuardName: "silence",
		ClusterID: uuid.New(),
		Result:    rollout.Result{Status: rollout.ResultCompleted},
		State:     rollout.GuardState{GuardName: "drain", Payload: []byte("silence-id")},
	})
	requireNonRetryable(t, err)
	assert.Contains(t, err.Error(), "drain")
	assert.Contains(t, err.Error(), "silence")
}

func TestActivities_GuardPost_SuccessAndPostFailure(t *testing.T) {
	t.Run("NodeGuardPostApply success returns nil", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerNodeGuard(t, reg, "act-ng-post-ok", NewFailingNodeGuardFactory("act-ng-post-ok"))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		err := a.NodeGuardPostApply(context.Background(), &temporal.NodeGuardPostApplyRequest{
			GuardName: "act-ng-post-ok",
			ClusterID: cl.id, NodeID: nid,
			Outcome: rollout.OutcomeApplied,
			State:   rollout.StatelessGuardState("act-ng-post-ok"),
		})
		require.NoError(t, err)
	})

	t.Run("NodeGuardPostApply guard failure is non-retryable", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerNodeGuard(t, reg, "act-ng-post-fail", NewFailingNodeGuardFactory("act-ng-post-fail"))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		err := a.NodeGuardPostApply(context.Background(), &temporal.NodeGuardPostApplyRequest{
			GuardName: "act-ng-post-fail",
			Params:    map[string]string{ParamFailPostApplyOn: "cp-1"},
			ClusterID: cl.id, NodeID: nid,
			Outcome: rollout.OutcomeApplied,
			State:   rollout.StatelessGuardState("act-ng-post-fail"),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "PostApply")
	})

	t.Run("ClusterGuardPostRollout success returns nil", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerClusterGuard(t, reg, "act-cg-post-ok", NewFailingClusterGuardFactory("act-cg-post-ok"))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.ClusterGuardPostRollout(context.Background(), &temporal.ClusterGuardPostRolloutRequest{
			GuardName: "act-cg-post-ok",
			ClusterID: uuid.New(),
			Result:    rollout.Result{Status: rollout.ResultCompleted},
			State:     rollout.StatelessGuardState("act-cg-post-ok"),
		})
		require.NoError(t, err)
	})

	t.Run("NodeGuardPostApply cluster-load failure stays retryable", func(t *testing.T) {
		// Postgres flake during cleanup must NOT permanently fail the
		// PostApply step; only the guard action itself wraps
		// non-retryable.
		reg := rollout.NewRegistry()
		registerNodeGuard(t, reg, "act-ng-post-load-flake", NewFailingNodeGuardFactory("act-ng-post-load-flake"))
		a := temporal.NewActivities(&fixedLoader{err: errors.New("postgres connection reset")}, reg)
		err := a.NodeGuardPostApply(context.Background(), &temporal.NodeGuardPostApplyRequest{
			GuardName: "act-ng-post-load-flake",
			ClusterID: uuid.New(), NodeID: uuid.New(),
			Outcome: rollout.OutcomeApplied,
			State:   rollout.StatelessGuardState("act-ng-post-load-flake"),
		})
		requireRetryable(t, err)
		assert.Contains(t, err.Error(), "load cluster")
	})

	t.Run("ClusterGuardPostRollout cluster-load failure stays retryable", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerClusterGuard(t, reg, "act-cg-post-load-flake", NewFailingClusterGuardFactory("act-cg-post-load-flake"))
		a := temporal.NewActivities(&fixedLoader{err: errors.New("postgres connection reset")}, reg)
		err := a.ClusterGuardPostRollout(context.Background(), &temporal.ClusterGuardPostRolloutRequest{
			GuardName: "act-cg-post-load-flake",
			ClusterID: uuid.New(),
			Result:    rollout.Result{Status: rollout.ResultCompleted},
			State:     rollout.StatelessGuardState("act-cg-post-load-flake"),
		})
		requireRetryable(t, err)
		assert.Contains(t, err.Error(), "load cluster")
	})
}

// ---------------------------------------------------------------------
// Plain-error activities (Validate, CheckPostCondition): no response
// type, so business and infrastructure failures both return a non-nil
// error. The workflow treats Validate failure as a cluster-phase abort.
// ---------------------------------------------------------------------

func TestActivities_Validate(t *testing.T) {
	t.Run("business error is non-retryable and wrapped", func(t *testing.T) {
		// Validate is a deterministic gate — op.Validate(cluster) returns
		// the same answer every time for the same (op, cluster), so a
		// failure must NOT retry. Default activity retry policy would
		// otherwise burn ~115s before surfacing the error.
		core := &cfgCore{name: "act-validate-biz", validate: func() error {
			return errors.New("kubeadm config does not parse")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.Validate(context.Background(), &temporal.ValidateRequest{
			OperationName: "act-validate-biz", ClusterID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "validate:")
		assert.Contains(t, err.Error(), "does not parse")
	})

	t.Run("success returns nil", func(t *testing.T) {
		core := &cfgCore{name: "act-validate-ok"}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		require.NoError(t, a.Validate(context.Background(), &temporal.ValidateRequest{
			OperationName: "act-validate-ok", ClusterID: uuid.New(),
		}))
	})

	t.Run("unregistered operation is non-retryable", func(t *testing.T) {
		// Registry miss = programmer/config error. Won't fix on retry.
		reg := rollout.NewRegistry()
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.Validate(context.Background(), &temporal.ValidateRequest{
			OperationName: "act-validate-missing", ClusterID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "not registered")
	})
}

func TestActivities_CheckPostCondition(t *testing.T) {
	t.Run("business error is non-retryable and wrapped", func(t *testing.T) {
		// Post-condition gate — same retry-is-wasteful rationale as
		// Validate. Probing the cluster's post-rollout state and
		// retrying re-probes the same state.
		core := &cfgCore{name: "act-postcond-biz", postCond: func() error {
			return errors.New("apiserver /readyz failing")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.CheckPostCondition(context.Background(), &temporal.CheckPostConditionRequest{
			OperationName: "act-postcond-biz", ClusterID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "check_post_condition:")
		assert.Contains(t, err.Error(), "/readyz failing")
	})

	t.Run("success returns nil", func(t *testing.T) {
		core := &cfgCore{name: "act-postcond-ok"}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		require.NoError(t, a.CheckPostCondition(context.Background(), &temporal.CheckPostConditionRequest{
			OperationName: "act-postcond-ok", ClusterID: uuid.New(),
		}))
	})
}

// RollbackNode is a cleanup-mutation activity: it runs on the cascade
// path, so its errors must be non-retryable (a retried teardown can act
// on a partially-restored node). executor_test.go covers the resolve-miss
// path; these cover the happy path and a guard-method failure.
func TestActivities_RollbackNode(t *testing.T) {
	t.Run("success returns nil", func(t *testing.T) {
		core := &cfgCore{name: "act-rollback-ok"} // rollback nil -> succeeds
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		require.NoError(t, a.RollbackNode(context.Background(), &temporal.RollbackRequest{
			OperationName: "act-rollback-ok", ClusterID: cl.id, NodeID: nid,
		}))
	})

	t.Run("rollback error is non-retryable", func(t *testing.T) {
		core := &cfgCore{name: "act-rollback-biz", rollback: func() error {
			return errors.New("could not restore manifest snapshot")
		}}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		err := a.RollbackNode(context.Background(), &temporal.RollbackRequest{
			OperationName: "act-rollback-biz", ClusterID: cl.id, NodeID: nid,
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "rollback:")
		assert.Contains(t, err.Error(), "restore manifest snapshot")
	})

	t.Run("cluster-load failure stays retryable", func(t *testing.T) {
		// A postgres flake during cascade rollback must NOT permanently
		// fail the rollback step. Only the rollback action itself wraps
		// non-retryable; the loader-failure path stays retryable so the
		// SDK can re-dispatch when postgres recovers.
		core := &cfgCore{name: "act-rollback-load-flake"}
		reg := rollout.NewRegistry()
		reg.RegisterOperation(rollout.Compile(core))
		a := temporal.NewActivities(&fixedLoader{err: errors.New("postgres connection reset")}, reg)
		err := a.RollbackNode(context.Background(), &temporal.RollbackRequest{
			OperationName: "act-rollback-load-flake", ClusterID: uuid.New(), NodeID: uuid.New(),
		})
		requireRetryable(t, err)
		assert.Contains(t, err.Error(), "load cluster")
	})
}

// ---------------------------------------------------------------------
// Preflight activities: resolve the named function from the registry,
// load the cluster (+ find the node, for node preflights), call it.
// ---------------------------------------------------------------------

func TestActivities_ClusterPreflight(t *testing.T) {
	t.Run("unregistered preflight is non-retryable", func(t *testing.T) {
		// Registry miss = programmer/config error. The name won't
		// appear in the registry on retry, so Temporal must not loop
		// for ~2min hoping it does.
		reg := rollout.NewRegistry()
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.ClusterPreflight(context.Background(), &temporal.ClusterPreflightRequest{
			PreflightName: "act-cpf-missing", ClusterID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "not registered")
	})

	t.Run("registered preflight that passes returns nil", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerClusterPreflight(t, reg, "act-cpf-ok", func(_ context.Context, _ rollout.Cluster, _ map[string]string) error {
			return nil
		})
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		require.NoError(t, a.ClusterPreflight(context.Background(), &temporal.ClusterPreflightRequest{
			PreflightName: "act-cpf-ok", ClusterID: uuid.New(),
		}))
	})

	t.Run("registered preflight that fails is non-retryable and surfaces the error", func(t *testing.T) {
		// Gate result is deterministic — the cluster's etcd quorum
		// state won't change just because Temporal re-dispatches. Wrap
		// non-retryable so operators get the result in ~one probe
		// duration vs the ~115s soak-measured default retry ceiling.
		reg := rollout.NewRegistry()
		registerClusterPreflight(t, reg, "act-cpf-fail", func(_ context.Context, _ rollout.Cluster, _ map[string]string) error {
			return errors.New("etcd not quorate")
		})
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.ClusterPreflight(context.Background(), &temporal.ClusterPreflightRequest{
			PreflightName: "act-cpf-fail", ClusterID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "etcd not quorate")
	})
}

func TestActivities_NodePreflight(t *testing.T) {
	t.Run("registered preflight that passes returns nil", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerNodePreflight(t, reg, "act-npf-ok", func(_ context.Context, _ rollout.Cluster, _ rollout.Node, _ map[string]string) error {
			return nil
		})
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		require.NoError(t, a.RunNodePreflight(context.Background(), &temporal.NodePreflightRequest{
			PreflightName: "act-npf-ok", ClusterID: cl.id, NodeID: nid,
		}))
	})

	t.Run("unregistered preflight is non-retryable", func(t *testing.T) {
		reg := rollout.NewRegistry()
		a := temporal.NewActivities(&fixedLoader{cluster: &fakeCluster{id: uuid.New()}}, reg)
		err := a.RunNodePreflight(context.Background(), &temporal.NodePreflightRequest{
			PreflightName: "act-npf-missing", ClusterID: uuid.New(), NodeID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "not registered")
	})

	t.Run("node not in cluster is non-retryable", func(t *testing.T) {
		// findNode failure = deterministic mismatch. Stale node UUID
		// won't appear in the cluster on retry.
		reg := rollout.NewRegistry()
		registerNodePreflight(t, reg, "act-npf-nonode", func(_ context.Context, _ rollout.Cluster, _ rollout.Node, _ map[string]string) error {
			return nil
		})
		cl, _ := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		err := a.RunNodePreflight(context.Background(), &temporal.NodePreflightRequest{
			PreflightName: "act-npf-nonode", ClusterID: cl.id, NodeID: uuid.New(),
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("registered preflight that fails is non-retryable", func(t *testing.T) {
		reg := rollout.NewRegistry()
		registerNodePreflight(t, reg, "act-npf-fail", func(_ context.Context, _ rollout.Cluster, _ rollout.Node, _ map[string]string) error {
			return errors.New("agent unreachable")
		})
		cl, nid := clusterWithNode()
		a := temporal.NewActivities(&fixedLoader{cluster: cl}, reg)
		err := a.RunNodePreflight(context.Background(), &temporal.NodePreflightRequest{
			PreflightName: "act-npf-fail", ClusterID: cl.id, NodeID: nid,
		})
		requireNonRetryable(t, err)
		assert.Contains(t, err.Error(), "agent unreachable")
	})
}

// A bare GuardState{} from a misbehaving guard is rejected at the
// activity layer rather than routing silently.
func TestNodeGuardPostApply_EmptyStateName_Rejected(t *testing.T) {
	t.Parallel()

	a := temporal.NewActivities(&nopLoader{}, rollout.NewRegistry())
	err := a.NodeGuardPostApply(context.Background(), &temporal.NodeGuardPostApplyRequest{
		GuardName: "silence",
		ClusterID: uuid.New(),
		NodeID:    uuid.New(),
		Outcome:   rollout.OutcomeApplied,
		State:     rollout.GuardState{}, // empty GuardName — the hazard
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty State.GuardName")
	assert.Contains(t, err.Error(), "silence",
		"error must name the targeted guard so operators can find the misbehaving fixture")
	assert.Contains(t, err.Error(), "StatelessGuardState",
		"error must point at the canonical fix")
}

// TestClusterGuardPostRollout_EmptyStateName_Rejected is the
// cluster-scoped sibling.
func TestClusterGuardPostRollout_EmptyStateName_Rejected(t *testing.T) {
	t.Parallel()

	a := temporal.NewActivities(&nopLoader{}, rollout.NewRegistry())
	err := a.ClusterGuardPostRollout(context.Background(), &temporal.ClusterGuardPostRolloutRequest{
		GuardName: "silence",
		ClusterID: uuid.New(),
		Result:    rollout.Result{Status: rollout.ResultCompleted},
		State:     rollout.GuardState{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty State.GuardName")
	assert.Contains(t, err.Error(), "silence")
}
