// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// DeleteOptions are the options for deleting a node.
type DeleteOptions struct {
	SkipRemoveFromCluster bool
	Reason                string
	skipLiveDeleteGuard   bool

	// ForceUnreachable treats the target node as potentially unreachable during
	// live-cluster removal. The workflow still attempts drain and graceful cleanup, but
	// tolerates failures that can occur when kubelet/agent is gone or the node is otherwise unreachable.
	// Which allows a delete or TeardownNode workflow to complete even if the node is unreachable.
	// Control-plane etcd member removal runs on a surviving control-plane node, so its failure is
	// not tolerated even when ForceUnreachable is set: leaving a stale member would endanger etcd
	// quorum.
	ForceUnreachable bool

	// Tearing down a control-plane node is not allowed by default,
	// But we do want to allow deleting control-plane nodes from the API.
	AllowCPNode bool

	// ControlPlaneControllerDelete routes a control-plane node delete through the
	// control-plane controller: the delete is recorded as intent and the
	// controller starts the teardown, one node at a time. Worker nodes are
	// unaffected. Set from UseControlPlaneControllerForControlPlane().
	ControlPlaneControllerDelete bool

	// SkipQuorumFloor exempts a controller-initiated delete from the
	// fault-tolerant floor. The floor refuses a delete that would leave fewer
	// than three joined control-plane nodes, which is right for a user shrinking
	// a control plane and wrong for a controller replacing a node: remediation
	// and rollout put the node back, and they already run their own etcd-quorum
	// precondition against live health. Without this a three-node control plane
	// could never remediate an unhealthy member.
	SkipQuorumFloor bool

	// If a non-nil MachineHealthIssue is provided it signals Forge to flag the instance for repair.
	MachineHealthIssue *MachineHealthIssue
}

// WithReason sets the reason for node deletion.
func WithReason(reason string) DeleteOpt {
	return func(o *DeleteOptions) {
		o.Reason = reason
	}
}

// WithSkipLiveDeleteGuard bypasses the live API deletion policy. Callers must
// enforce their own deletion safety invariants before using this option.
func WithSkipLiveDeleteGuard() DeleteOpt {
	return func(o *DeleteOptions) {
		o.skipLiveDeleteGuard = true
	}
}

// WithAllowCPNode allows deleting control-plane nodes when set to true.
// By default, tearing down control-plane nodes is not allowed.
// WithSkipQuorumFloor exempts a controller-initiated delete from the
// fault-tolerant floor. Only for callers that replace the node and enforce
// their own quorum precondition.
func WithSkipQuorumFloor() DeleteOpt {
	return func(o *DeleteOptions) {
		o.SkipQuorumFloor = true
	}
}

func WithAllowCPNode(allow bool) DeleteOpt {
	return func(o *DeleteOptions) {
		o.AllowCPNode = allow
	}
}

// WithForceUnreachable treats the target node as potentially unreachable during
// live-cluster removal. The workflow still attempts drain and graceful cleanup, but
// tolerates failures that can occur when kubelet/agent is gone or the node is otherwise unreachable.
// Which allows a delete or TeardownNode workflow to complete even if the node is unreachable.
// Control-plane etcd member removal runs on a surviving control-plane node, so its failure is
// not tolerated even when ForceUnreachable is set: leaving a stale member would endanger etcd
// quorum.
func WithForceUnreachable(forceUnreachable bool) DeleteOpt {
	return func(o *DeleteOptions) {
		o.ForceUnreachable = forceUnreachable
	}
}

// WithControlPlaneControllerDelete records a control-plane node delete as intent
// for the control-plane controller to act on instead of starting the teardown
// inline. It has no effect on worker nodes.
func WithControlPlaneControllerDelete(useController bool) DeleteOpt {
	return func(o *DeleteOptions) {
		o.ControlPlaneControllerDelete = useController
	}
}

// WithMachineHealthIssue signals Forge to flag the instance for repair.
// If a non-nil MachineHealthIssue is provided, it will be included in the teardown workflow input.
func WithMachineHealthIssue(machineHealthIssue *MachineHealthIssue) DeleteOpt {
	return func(o *DeleteOptions) {
		o.MachineHealthIssue = machineHealthIssue
	}
}

func (o *DeleteOptions) toWorkflowInput() TeardownNodeInput {
	input := TeardownNodeInput{}
	if o.SkipRemoveFromCluster {
		input.SkipRemoveFromCluster = o.SkipRemoveFromCluster
	}
	if o.Reason != "" {
		input.Reason = o.Reason
	}
	if o.ForceUnreachable {
		input.ForceUnreachable = o.ForceUnreachable
	}
	if o.AllowCPNode {
		input.AllowCPNode = o.AllowCPNode
	}
	if o.MachineHealthIssue != nil {
		input.MachineHealthIssue = o.MachineHealthIssue
	}

	return input
}

type DeleteOpt func(*DeleteOptions)

// Delete marks a node for deletion inside a database transaction.
func Delete(ctx context.Context, database db.DB, nodeID uuid.UUID, opts ...DeleteOpt) error {
	return database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return DeleteWithTx(ctx, tx, nodeID, opts...)
	})
}

// DeleteWithTx marks a node for deletion using the provided transaction.
// Callers already inside RunInTx should use this to avoid nested transactions.
func DeleteWithTx(ctx context.Context, tx db.Tx, nodeID uuid.UUID, opts ...DeleteOpt) error {
	options := DeleteOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	var dbNode db.Node
	var err error
	if options.skipLiveDeleteGuard {
		dbNode, err = tx.GetNodeForUpdate(ctx, nodeID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return fmt.Errorf("node not found for deletion: %w", err)
			}
			return fmt.Errorf("failed to get node: %w", err)
		}
	} else {
		dbNode, err = guardLiveNodeDelete(ctx, tx, nodeID)
		if err != nil {
			return err
		}
	}

	// Control-plane deletes are owned by the control-plane controller when the
	// flag is on: record the intent and let it start the teardown, one node at a
	// time. Worker deletes stay direct, and so do control-plane deletes with the
	// flag off.
	if options.ControlPlaneControllerDelete && dbNode.ControlPlane {
		return recordControlPlaneDeleteRequest(ctx, tx, dbNode, options)
	}

	dbNode.Conditions.Deleting = db.NewCondition(true, "Node deletion in progress")
	// A request recorded while the control-plane-controller path was on is
	// absorbed here rather than refused: with the flag off nothing would ever
	// promote it, so the node would sit marked forever. The direct delete is that
	// promotion.
	dbNode.Conditions.DeleteRequested = db.NewCondition(false, "Teardown started")
	dbNode.DeleteRequest = nil

	if _, err := tx.UpdateNode(ctx, &dbNode); err != nil {
		return fmt.Errorf("failed to update node: %w", err)
	}

	workflowID := constants.TeardownNodeWorkflowPrefix + dbNode.ID.String()

	input := options.toWorkflowInput()
	input.NodeID = dbNode.ID

	if _, err = tx.CreateWorkflowOutboxEntry(ctx, constants.TeardownNodeWorkflowType, workflowID, input); err != nil {
		return fmt.Errorf("failed to create workflow outbox entry: %w", err)
	}

	return nil
}

// DeleteNodePool marks a node pool for deletion inside a database transaction.
func DeleteNodePool(ctx context.Context, database db.DB, nodePoolID uuid.UUID) error {
	return database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return DeleteNodePoolWithTx(ctx, tx, nodePoolID)
	})
}

// DeleteNodePoolWithTx marks a node pool for deletion using the provided
// transaction. Callers already inside RunInTx should use this to avoid nested
// transactions. It enforces the live delete guard (cluster lock + last joined
// control-plane check) before marking the pool deleting.
func DeleteNodePoolWithTx(ctx context.Context, tx db.Tx, nodePoolID uuid.UUID) error {
	dbNodePool, err := guardLiveNodePoolDelete(ctx, tx, nodePoolID)
	if err != nil {
		return err
	}

	dbNodePool.Conditions.Deleting = db.NewCondition(true, "Node pool deletion in progress")

	if _, err := tx.UpdateNodePool(ctx, &dbNodePool); err != nil {
		return fmt.Errorf("failed to update node pool: %w", err)
	}

	workflowID := constants.TeardownNodePoolWorkflowPrefix + dbNodePool.ID.String()
	if _, err = tx.CreateWorkflowOutboxEntry(ctx, "TeardownNodePool", workflowID, map[string]interface{}{
		"nodePoolID": dbNodePool.ID,
	}); err != nil {
		return fmt.Errorf("failed to create workflow outbox entry: %w", err)
	}

	return nil
}
