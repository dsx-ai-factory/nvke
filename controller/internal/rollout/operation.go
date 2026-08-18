// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"context"
)

// Operation is the required contract. Every other capability is
// optional and auto-detected by Compile.
type Operation interface {
	// Name returns a stable identifier for logs, metrics, and registry keys.
	Name() string

	// ApplyToNode applies the operation to one node. The returned
	// Outcome is authoritative — the framework switches on Outcome,
	// not on the error. Operations whose apply did not succeed MUST
	// set FailedRestored or FailedNotRestored; the zero value
	// (Unspecified) routes through the fail-closed cascade arm.
	ApplyToNode(ctx context.Context, node Node, payload ApplyToNodePayload) (Outcome, error)
}

// ApplyToNodePayloadBuilder builds a payload once per Execute, delivered
// byte-identical to every ApplyToNode and Rollback.
type ApplyToNodePayloadBuilder interface {
	BuildApplyToNodePayload(ctx context.Context, cluster Cluster) (ApplyToNodePayload, error)
}

// SatisfactionChecker short-circuits: IsSatisfied=true returns
// ResultAlreadySatisfied without touching any node. Distinct from
// per-node OutcomeUnchanged.
type SatisfactionChecker interface {
	IsSatisfied(ctx context.Context, cluster Cluster) (bool, error)
}

// Validator runs before ClusterPreflights. Failure aborts the rollout.
type Validator interface {
	Validate(ctx context.Context, cluster Cluster) error
}

// RollbackHandler is the cluster-level recovery the framework drives: on
// a cascade it undoes ApplyToNode across the nodes that already applied
// successfully, in reverse apply order. This is distinct from an
// operation restoring a single node itself (OutcomeFailedRestored) — a
// cascade is triggered by OutcomeFailedNotRestored (or the
// halt-on-any-failure policy), not by a clean self-restore. Errors are
// logged; the cascade always completes.
type RollbackHandler interface {
	Rollback(ctx context.Context, node Node, payload ApplyToNodePayload) error
}

// PostConditionChecker runs once after the final node's guards.
// Failure does NOT trigger rollback; it surfaces as a non-success Result.
type PostConditionChecker interface {
	CheckPostCondition(ctx context.Context, cluster Cluster) error
}

// CompiledOperation is the value the executor consumes. Construct via Compile;
// nil function fields = capability absent.
type CompiledOperation struct {
	name                    string
	applyToNode             func(ctx context.Context, node Node, payload ApplyToNodePayload) (Outcome, error)
	buildApplyToNodePayload func(ctx context.Context, cluster Cluster) (ApplyToNodePayload, error)
	isSatisfied             func(ctx context.Context, cluster Cluster) (bool, error)
	validate                func(ctx context.Context, cluster Cluster) error
	rollback                func(ctx context.Context, node Node, payload ApplyToNodePayload) error
	checkPostCondition      func(ctx context.Context, cluster Cluster) error
}

// Compile wraps a value satisfying Operation. Optional
// capabilities are auto-detected; adding one is backward compatible.
func Compile(core Operation) CompiledOperation {
	op := CompiledOperation{
		name:        core.Name(),
		applyToNode: core.ApplyToNode,
	}
	if v, ok := core.(ApplyToNodePayloadBuilder); ok {
		op.buildApplyToNodePayload = v.BuildApplyToNodePayload
	}
	if v, ok := core.(SatisfactionChecker); ok {
		op.isSatisfied = v.IsSatisfied
	}
	if v, ok := core.(Validator); ok {
		op.validate = v.Validate
	}
	if v, ok := core.(RollbackHandler); ok {
		op.rollback = v.Rollback
	}
	if v, ok := core.(PostConditionChecker); ok {
		op.checkPostCondition = v.CheckPostCondition
	}
	return op
}

func (op CompiledOperation) Name() string { return op.name }

// Capabilities is snapshotted once at Execute entry so dispatch
// decisions are stable across workflow replay.
type Capabilities struct {
	HasApplyToNodePayloadBuilder bool
	HasSatisfactionChecker       bool
	HasValidator                 bool
	HasRollbackHandler           bool
	HasPostConditionChecker      bool
}

func (op CompiledOperation) Capabilities() Capabilities {
	return Capabilities{
		HasApplyToNodePayloadBuilder: op.buildApplyToNodePayload != nil,
		HasSatisfactionChecker:       op.isSatisfied != nil,
		HasValidator:                 op.validate != nil,
		HasRollbackHandler:           op.rollback != nil,
		HasPostConditionChecker:      op.checkPostCondition != nil,
	}
}

func (op CompiledOperation) ApplyToNode(ctx context.Context, node Node, payload ApplyToNodePayload) (Outcome, error) {
	if op.applyToNode == nil {
		return OutcomeUnspecified, nil
	}
	return op.applyToNode(ctx, node, payload)
}

func (op CompiledOperation) BuildApplyToNodePayload(ctx context.Context, cluster Cluster) (ApplyToNodePayload, error) {
	if op.buildApplyToNodePayload == nil {
		return nil, nil
	}
	return op.buildApplyToNodePayload(ctx, cluster)
}

// IsSatisfied returns (false, nil) when no SatisfactionChecker is wired.
func (op CompiledOperation) IsSatisfied(ctx context.Context, cluster Cluster) (bool, error) {
	if op.isSatisfied == nil {
		return false, nil
	}
	return op.isSatisfied(ctx, cluster)
}

func (op CompiledOperation) Validate(ctx context.Context, cluster Cluster) error {
	if op.validate == nil {
		return nil
	}
	return op.validate(ctx, cluster)
}

func (op CompiledOperation) Rollback(ctx context.Context, node Node, payload ApplyToNodePayload) error {
	if op.rollback == nil {
		return nil
	}
	return op.rollback(ctx, node, payload)
}

func (op CompiledOperation) CheckPostCondition(ctx context.Context, cluster Cluster) error {
	if op.checkPostCondition == nil {
		return nil
	}
	return op.checkPostCondition(ctx, cluster)
}
