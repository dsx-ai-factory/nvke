// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

// coreOnly satisfies only Operation; no optional capabilities.
type coreOnly struct{}

func (coreOnly) Name() string { return "core-only" }
func (coreOnly) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

// fullCaps satisfies Operation plus every optional capability.
type fullCaps struct {
	payload        []byte
	isSatReturn    bool
	validateReturn error
	rollbackReturn error
	postCondReturn error
	applyReturn    rollout.Outcome
	applyErrReturn error
}

func (f *fullCaps) Name() string { return "full-caps" }

func (f *fullCaps) ApplyToNode(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	return f.applyReturn, f.applyErrReturn
}

func (f *fullCaps) BuildApplyToNodePayload(_ context.Context, _ rollout.Cluster) (rollout.ApplyToNodePayload, error) {
	return rollout.ApplyToNodePayload(f.payload), nil
}

func (f *fullCaps) IsSatisfied(_ context.Context, _ rollout.Cluster) (bool, error) {
	return f.isSatReturn, nil
}

func (f *fullCaps) Validate(_ context.Context, _ rollout.Cluster) error {
	return f.validateReturn
}

func (f *fullCaps) Rollback(_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload) error {
	return f.rollbackReturn
}

func (f *fullCaps) CheckPostCondition(_ context.Context, _ rollout.Cluster) error {
	return f.postCondReturn
}

func TestCompile_CoreOnly_HasNoCapabilities(t *testing.T) {
	op := rollout.Compile(coreOnly{})

	assert.Equal(t, "core-only", op.Name())
	assert.False(t, op.Capabilities().HasApplyToNodePayloadBuilder)
	assert.False(t, op.Capabilities().HasSatisfactionChecker)
	assert.False(t, op.Capabilities().HasValidator)
	assert.False(t, op.Capabilities().HasRollbackHandler)
	assert.False(t, op.Capabilities().HasPostConditionChecker)

	// Absent capabilities default to safe nil/no-op behavior.
	ctx := context.Background()
	art, err := op.BuildApplyToNodePayload(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, art)

	sat, err := op.IsSatisfied(ctx, nil)
	require.NoError(t, err)
	assert.False(t, sat)

	assert.NoError(t, op.Validate(ctx, nil))
	assert.NoError(t, op.Rollback(ctx, nil, nil))
	assert.NoError(t, op.CheckPostCondition(ctx, nil))
}

func TestCompile_AutoWiresEveryCapability(t *testing.T) {
	op := rollout.Compile(&fullCaps{})

	assert.Equal(t, "full-caps", op.Name())
	assert.True(t, op.Capabilities().HasApplyToNodePayloadBuilder)
	assert.True(t, op.Capabilities().HasSatisfactionChecker)
	assert.True(t, op.Capabilities().HasValidator)
	assert.True(t, op.Capabilities().HasRollbackHandler)
	assert.True(t, op.Capabilities().HasPostConditionChecker)
}

// TestCompile_ForwardsReturnValues proves the compiled Operation delegates to
// the underlying implementation — the capability bits flipping true is not
// enough; a mis-wired dispatch that swallowed return values or errors would
// still pass the capability-bit checks. Each method returns a distinctive
// value/error so a wrong wiring is observable.
func TestCompile_ForwardsReturnValues(t *testing.T) {
	wantValidate := errors.New("validate boom")
	wantRollback := errors.New("rollback boom")
	wantPostCond := errors.New("postcondition boom")
	wantApplyErr := errors.New("apply boom")

	op := rollout.Compile(&fullCaps{
		payload:        []byte("payload-xyz"),
		isSatReturn:    true,
		validateReturn: wantValidate,
		rollbackReturn: wantRollback,
		postCondReturn: wantPostCond,
		applyReturn:    rollout.OutcomeApplied,
		applyErrReturn: wantApplyErr,
	})

	ctx := context.Background()

	art, err := op.BuildApplyToNodePayload(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, rollout.ApplyToNodePayload([]byte("payload-xyz")), art)

	sat, err := op.IsSatisfied(ctx, nil)
	require.NoError(t, err)
	assert.True(t, sat)

	require.ErrorIs(t, op.Validate(ctx, nil), wantValidate)
	require.ErrorIs(t, op.Rollback(ctx, nil, nil), wantRollback)
	require.ErrorIs(t, op.CheckPostCondition(ctx, nil), wantPostCond)

	outcome, err := op.ApplyToNode(ctx, nil, nil)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	require.ErrorIs(t, err, wantApplyErr)
}

// TestOperation_Capabilities locks in the snapshot contract: the
// returned struct is a faithful mirror of the underlying capability
// bits.
func TestOperation_Capabilities(t *testing.T) {
	bare := rollout.Compile(coreOnly{})
	assert.Equal(t, rollout.Capabilities{}, bare.Capabilities(),
		"bare Operation has no optional capabilities")

	full := rollout.Compile(&fullCaps{})
	assert.Equal(t, rollout.Capabilities{
		HasApplyToNodePayloadBuilder: true,
		HasSatisfactionChecker:       true,
		HasValidator:                 true,
		HasRollbackHandler:           true,
		HasPostConditionChecker:      true,
	}, full.Capabilities())
}

// TestRolloutSpec_Validate covers the structural invariants the
// executors rely on. See spec.go on why registry-presence checks are
// not part of Validate's scope.
func TestRolloutSpec_Validate(t *testing.T) {
	cases := []struct {
		name string
		spec rollout.RolloutSpec
		ok   bool
	}{
		{name: "empty strategy is accepted (defaults to Serial)", spec: rollout.RolloutSpec{}, ok: true},
		{name: "Serial is accepted", spec: rollout.RolloutSpec{Strategy: rollout.StrategySerial}, ok: true},
		{name: "unknown strategy is rejected", spec: rollout.RolloutSpec{Strategy: "Parallel"}, ok: false},

		// MinHealthyReplicas.MinHealthy structural check.
		{name: "MinHealthy=0 is accepted (no constraint)", spec: rollout.RolloutSpec{MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: 0}}, ok: true},
		{name: "positive MinHealthy is accepted", spec: rollout.RolloutSpec{MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: 2}}, ok: true},
		{name: "negative MinHealthy is rejected", spec: rollout.RolloutSpec{MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: -1}}, ok: false},

		// RollbackPolicy enum check.
		{name: "empty rollback policy is accepted (default)", spec: rollout.RolloutSpec{RollbackPolicy: ""}, ok: true},
		{name: "CascadeOnUnrecoverable is accepted", spec: rollout.RolloutSpec{RollbackPolicy: rollout.RollbackCascadeOnUnrecoverable}, ok: true},
		{name: "HaltOnAnyFailure is accepted", spec: rollout.RolloutSpec{RollbackPolicy: rollout.RollbackHaltOnAnyFailure}, ok: true},
		{name: "typo rollback policy is rejected", spec: rollout.RolloutSpec{RollbackPolicy: "CascadeOnUnrecoverabl"}, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// TestRegisterNodeGuard_PanicsOnDuplicate locks in the panic-on-dup
// safety of the RegisterNodeGuard helper — silent overwrite of a guard
// factory at init() time would route dispatch to the wrong
// implementation. Parallels the same contract on
// temporal.RegisterOperation.
func TestRegisterNodeGuard_PanicsOnDuplicate(t *testing.T) {
	const name = "test-dup-node-guard"
	reg := rollout.NewRegistry()

	factory := func(_ map[string]string) (rollout.NodeGuard, error) {
		panic("factory must not be called")
	}
	reg.RegisterNodeGuard(name, factory)
	assert.Panics(t, func() { reg.RegisterNodeGuard(name, factory) },
		"second registration under the same name MUST panic")
}

// TestRegisterClusterGuard_PanicsOnDuplicate is the cluster-guard mirror
// of TestRegisterNodeGuard_PanicsOnDuplicate.
func TestRegisterClusterGuard_PanicsOnDuplicate(t *testing.T) {
	const name = "test-dup-cluster-guard"
	reg := rollout.NewRegistry()

	factory := func(_ map[string]string) (rollout.ClusterGuard, error) {
		panic("factory must not be called")
	}
	reg.RegisterClusterGuard(name, factory)
	assert.Panics(t, func() { reg.RegisterClusterGuard(name, factory) },
		"second registration under the same name MUST panic")
}

// TestRegisterGuard_PanicsOnEmptyNameOrNilFactory locks in the N1
// fail-fast checks: an empty name or a nil factory at init() time
// would otherwise surface much later as a confusing dispatch-time
// lookup miss or nil-call panic. Catches both node-guard and
// cluster-guard variants.
func TestRegisterGuard_PanicsOnEmptyNameOrNilFactory(t *testing.T) {
	reg := rollout.NewRegistry()
	nodeFactory := func(_ map[string]string) (rollout.NodeGuard, error) {
		panic("factory must not be called")
	}
	clusterFactory := func(_ map[string]string) (rollout.ClusterGuard, error) {
		panic("factory must not be called")
	}

	assert.Panics(t, func() { reg.RegisterNodeGuard("", nodeFactory) },
		"RegisterNodeGuard must panic on empty name")
	assert.Panics(t, func() { reg.RegisterNodeGuard("test-nil-factory-node", nil) },
		"RegisterNodeGuard must panic on nil factory")
	assert.Panics(t, func() { reg.RegisterClusterGuard("", clusterFactory) },
		"RegisterClusterGuard must panic on empty name")
	assert.Panics(t, func() { reg.RegisterClusterGuard("test-nil-factory-cluster", nil) },
		"RegisterClusterGuard must panic on nil factory")
}
