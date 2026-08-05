// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package temporal

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	cwepreflights "github.com/NVIDIA/nke/controller/internal/rollout/preflights"
)

// minimalOp is the smallest possible Operation for factory tests.
// It returns OutcomeUnchanged from ApplyToNode so the factory's
// downstream Execute call would be a no-op against a real cluster.
type minimalOp struct{ name string }

func (op *minimalOp) Name() string { return op.name }
func (op *minimalOp) ApplyToNode(
	_ context.Context, _ rollout.Node, _ rollout.ApplyToNodePayload,
) (rollout.Outcome, error) {
	return rollout.OutcomeUnchanged, nil
}

// recordingRegisterer is a WorkflowRegisterer that records the workflow
// names (and functions) handed to it, standing in for a worker.Worker.
type recordingRegisterer struct {
	fns map[string]any
}

func (r *recordingRegisterer) RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions) {
	if r.fns == nil {
		r.fns = map[string]any{}
	}
	r.fns[options.Name] = w
}

func (r *recordingRegisterer) has(name string) bool {
	_, ok := r.fns[name]
	return ok
}

func TestRegisterRolloutWorkflow_NilImpl_Panics(t *testing.T) {
	assert.PanicsWithValue(
		t,
		"temporal rollout: WorkflowConfig.Impl is nil",
		func() {
			RegisterRolloutWorkflow(&recordingRegisterer{}, rollout.NewRegistry(), nil,
				WorkflowConfig{Impl: nil, Spec: Static(rollout.RolloutSpec{})})
		},
	)
}

func TestRegisterRolloutWorkflow_NilSpec_Panics(t *testing.T) {
	assert.PanicsWithValue(
		t,
		"temporal rollout: WorkflowConfig.Spec is nil (use rollouttemporal.Static for constant specs)",
		func() {
			RegisterRolloutWorkflow(&recordingRegisterer{}, rollout.NewRegistry(), nil,
				WorkflowConfig{Impl: &minimalOp{name: "OpNoSpec"}, Spec: nil})
		},
	)
}

func TestRegisterRolloutWorkflow_EmptyName_Panics(t *testing.T) {
	assert.PanicsWithValue(
		t,
		"temporal rollout: operation Name() returned empty string",
		func() {
			RegisterRolloutWorkflow(&recordingRegisterer{}, rollout.NewRegistry(), nil,
				WorkflowConfig{Impl: &minimalOp{name: ""}, Spec: Static(rollout.RolloutSpec{})})
		},
	)
}

func TestRegisterRolloutWorkflow_RegistersOperationAndWorkflow(t *testing.T) {
	reg := rollout.NewRegistry()
	rec := &recordingRegisterer{}

	wf := RegisterRolloutWorkflow(rec, reg, nil, WorkflowConfig{
		Impl: &minimalOp{name: "TestOp"},
		Spec: Static(rollout.RolloutSpec{Strategy: rollout.StrategySerial}),
	})

	// Operation must be in the activity-side registry so activities can
	// resolve by name.
	op, ok := reg.LookupOperation("TestOp")
	require.True(t, ok, "operation should be registered for activities to resolve")
	assert.Equal(t, "TestOp", op.Name())

	// Workflow must be registered with the worker, and the returned
	// handle is non-nil (so a test environment can execute it).
	assert.True(t, rec.has("TestOp"), "workflow should be registered with the worker")
	assert.NotNil(t, wf, "RegisterRolloutWorkflow must return the workflow handle")
}

func TestRegisterRolloutWorkflow_DuplicateName_Panics(t *testing.T) {
	reg := rollout.NewRegistry()
	rec := &recordingRegisterer{}

	RegisterRolloutWorkflow(rec, reg, nil, WorkflowConfig{
		Impl: &minimalOp{name: "DupOp"},
		Spec: Static(rollout.RolloutSpec{}),
	})

	// Same name a second time — the operation registry panics on the
	// duplicate registration.
	assert.Panics(t, func() {
		RegisterRolloutWorkflow(rec, reg, nil, WorkflowConfig{
			Impl: &minimalOp{name: "DupOp"},
			Spec: Static(rollout.RolloutSpec{}),
		})
	})
}

func TestRegisterRolloutWorkflow_DifferentNames_BothRegistered(t *testing.T) {
	reg := rollout.NewRegistry()
	rec := &recordingRegisterer{}

	RegisterRolloutWorkflow(rec, reg, nil, WorkflowConfig{Impl: &minimalOp{name: "OpA"}, Spec: Static(rollout.RolloutSpec{})})
	RegisterRolloutWorkflow(rec, reg, nil, WorkflowConfig{Impl: &minimalOp{name: "OpB"}, Spec: Static(rollout.RolloutSpec{})})

	_, okA := reg.LookupOperation("OpA")
	_, okB := reg.LookupOperation("OpB")
	assert.True(t, okA, "OpA should be registered")
	assert.True(t, okB, "OpB should be registered")

	assert.True(t, rec.has("OpA"), "OpA workflow should be registered")
	assert.True(t, rec.has("OpB"), "OpB workflow should be registered")
}

// ----------------------------------------------------------------------
// effectiveSpecFn: SuppressDefaultPreflights + mutator composition
// ----------------------------------------------------------------------

// fakeFactoryCluster is the smallest rollout.Cluster impl that the
// spec-mutator path needs — Spec functions only see a Cluster, and
// Static/derived specs don't actually use the fields here, but the type
// must satisfy the interface.
type fakeFactoryCluster struct{}

func (fakeFactoryCluster) ID() uuid.UUID             { return uuid.Nil }
func (fakeFactoryCluster) Name() string              { return "fake" }
func (fakeFactoryCluster) KubernetesVersion() string { return "v1.30.0" }
func (fakeFactoryCluster) ControlPlaneVIP() string   { return "10.0.0.1:6443" }
func (fakeFactoryCluster) ControlPlaneNodes() []rollout.Node {
	return nil
}
func (fakeFactoryCluster) Nodes() []rollout.Node { return nil }

func TestEffectiveSpecFn_DefaultsArePrependedWhenNotSuppressed(t *testing.T) {
	// Consumer-supplied preflights — must run AFTER the defaults.
	consumerCP := []rollout.ClusterPreflightRef{{Name: "consumer.cluster.check"}}
	consumerNode := []rollout.NodePreflightRef{{Name: "consumer.node.check"}}

	specFn := effectiveSpecFn(WorkflowConfig{
		Impl: &minimalOp{name: "OpWithDefaults"},
		Spec: Static(rollout.RolloutSpec{
			ClusterPreflights: consumerCP,
			NodePreflights:    consumerNode,
		}),
		// SuppressDefaultPreflights left as zero value (false) — defaults on.
	}, cwepreflights.PrependDefaults)
	spec := specFn(fakeFactoryCluster{})

	expectedCP := append([]rollout.ClusterPreflightRef{}, cwepreflights.DefaultClusterPreflights()...)
	expectedCP = append(expectedCP, consumerCP...)
	assert.Equal(t, expectedCP, spec.ClusterPreflights,
		"defaults must be PREPENDED so the safety floor runs first")

	expectedNode := append([]rollout.NodePreflightRef{}, cwepreflights.DefaultNodePreflights()...)
	expectedNode = append(expectedNode, consumerNode...)
	assert.Equal(t, expectedNode, spec.NodePreflights,
		"defaults must be PREPENDED so the safety floor runs first")
}

func TestEffectiveSpecFn_SuppressDefaultPreflights_LeavesSpecUntouched(t *testing.T) {
	consumerCP := []rollout.ClusterPreflightRef{{Name: "consumer.cluster.check"}}
	consumerNode := []rollout.NodePreflightRef{{Name: "consumer.node.check"}}

	specFn := effectiveSpecFn(WorkflowConfig{
		Impl: &minimalOp{name: "OpSuppressed"},
		Spec: Static(rollout.RolloutSpec{
			ClusterPreflights: consumerCP,
			NodePreflights:    consumerNode,
		}),
		SuppressDefaultPreflights: true,
	}, cwepreflights.PrependDefaults)
	spec := specFn(fakeFactoryCluster{})

	assert.Equal(t, consumerCP, spec.ClusterPreflights,
		"suppression must skip default prepend — consumer slice unchanged")
	assert.Equal(t, consumerNode, spec.NodePreflights,
		"suppression must skip default prepend — consumer slice unchanged")
}

func TestEffectiveSpecFn_DefaultsAreInstalledOnEmptySpec(t *testing.T) {
	specFn := effectiveSpecFn(WorkflowConfig{
		Impl: &minimalOp{name: "OpEmpty"},
		Spec: Static(rollout.RolloutSpec{}), // no consumer preflights
	}, cwepreflights.PrependDefaults)
	spec := specFn(fakeFactoryCluster{})

	assert.Equal(t, cwepreflights.DefaultClusterPreflights(), spec.ClusterPreflights)
	assert.Equal(t, cwepreflights.DefaultNodePreflights(), spec.NodePreflights)
}

func TestEffectiveSpecFn_NoMutatorReturnsInnerUnchanged(t *testing.T) {
	// With no mutator supplied, effectiveSpecFn returns the raw Spec —
	// a no-op pass-through. Guards against a regression where a stale or
	// default mutator silently runs.
	consumerCP := []rollout.ClusterPreflightRef{{Name: "consumer.cp"}}
	specFn := effectiveSpecFn(WorkflowConfig{
		Impl: &minimalOp{name: "OpNoMutator"},
		Spec: Static(rollout.RolloutSpec{ClusterPreflights: consumerCP}),
	}, nil)

	spec := specFn(fakeFactoryCluster{})
	assert.Equal(t, consumerCP, spec.ClusterPreflights,
		"no mutator supplied → spec returned verbatim")
	assert.Nil(t, spec.NodePreflights)
}

func TestWrapWithMutator_IsPureAcrossCalls(t *testing.T) {
	// Replay-safety guard: wrapWithMutator must produce a pure function.
	// Two calls with the same inner+cluster must produce equal specs — no
	// clock, no env, no growing slices, no hidden state that would diverge
	// on a Temporal workflow replay.
	inner := func(rollout.Cluster) rollout.RolloutSpec {
		return rollout.RolloutSpec{
			ClusterPreflights: []rollout.ClusterPreflightRef{{Name: "consumer.cp"}},
			NodePreflights:    []rollout.NodePreflightRef{{Name: "consumer.n"}},
		}
	}
	wrapped := wrapWithMutator(inner, cwepreflights.PrependDefaults)

	a := wrapped(fakeFactoryCluster{})
	b := wrapped(fakeFactoryCluster{})
	assert.Equal(t, a, b)
	assert.Len(t, a.ClusterPreflights, len(cwepreflights.DefaultClusterPreflights())+1,
		"prepended defaults + consumer entry")
	assert.Len(t, a.NodePreflights, len(cwepreflights.DefaultNodePreflights())+1,
		"prepended defaults + consumer entry")
}

func TestRegisterRolloutWorkflow_NonDeterministicSpec_Panics(t *testing.T) {
	// Spec function captures a counter that bumps on every call — the
	// most common impurity shape (cf. a buggy closure that derives a
	// field from a clock / random / env). The two registration-time calls
	// produce different RolloutSpec values, so the assertion must fire.
	calls := 0
	naughtySpec := func(_ rollout.Cluster) rollout.RolloutSpec {
		calls++
		return rollout.RolloutSpec{
			MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: calls},
		}
	}
	defer func() {
		r := recover()
		require.NotNil(t, r, "RegisterRolloutWorkflow must panic on non-deterministic Spec")
		msg, ok := r.(string)
		require.True(t, ok, "panic value should be a string, got %T", r)
		assert.Contains(t, msg, "non-deterministic")
		assert.Contains(t, msg, "OpImpure")
	}()
	RegisterRolloutWorkflow(&recordingRegisterer{}, rollout.NewRegistry(), nil, WorkflowConfig{
		Impl: &minimalOp{name: "OpImpure"},
		Spec: naughtySpec,
	})
}

func TestRegisterRolloutWorkflow_NonDeterministicMutator_Panics(t *testing.T) {
	calls := 0
	naughtyMutator := func(_ rollout.Cluster, spec rollout.RolloutSpec) rollout.RolloutSpec {
		calls++
		spec.MinHealthyReplicas.MinHealthy = calls
		return spec
	}

	defer func() {
		r := recover()
		require.NotNil(t, r, "RegisterRolloutWorkflow must panic on a non-deterministic mutator")
		msg, ok := r.(string)
		require.True(t, ok, "panic value should be a string, got %T", r)
		assert.Contains(t, msg, "non-deterministic")
		assert.Contains(t, msg, "OpImpureMutator")
	}()
	RegisterRolloutWorkflow(&recordingRegisterer{}, rollout.NewRegistry(), naughtyMutator, WorkflowConfig{
		Impl: &minimalOp{name: "OpImpureMutator"},
		Spec: Static(rollout.RolloutSpec{}),
	})
}

func TestRegisterRolloutWorkflow_SpecPanicOnSentinel_WrapsWithContext(t *testing.T) {
	// Spec function dereferences a cluster field that's nil on the
	// sentinel — the purity-check wrapper recovers and re-panics with a
	// clear message pointing back at the operation name.
	panicySpec := func(c rollout.Cluster) rollout.RolloutSpec {
		// purityCheckCluster returns no CPs; indexing [0] panics.
		nodes := c.ControlPlaneNodes()
		_ = nodes[0].Name()
		return rollout.RolloutSpec{}
	}
	defer func() {
		r := recover()
		require.NotNil(t, r, "RegisterRolloutWorkflow must panic when Spec panics on the sentinel")
		msg, ok := r.(string)
		require.True(t, ok, "panic value should be a string, got %T", r)
		assert.Contains(t, msg, "panicked on the registration-time purity check")
		assert.Contains(t, msg, "OpPanicSpec")
	}()
	RegisterRolloutWorkflow(&recordingRegisterer{}, rollout.NewRegistry(), nil, WorkflowConfig{
		Impl: &minimalOp{name: "OpPanicSpec"},
		Spec: panicySpec,
	})
}
