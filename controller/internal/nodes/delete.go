// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
	// quorum. Use AllowQuorumLoss/AllowSingleVoter to override the dry-run safety checks instead.
	ForceUnreachable bool
	AllowQuorumLoss  bool
	AllowSingleVoter bool

	// Tearing down a control-plane node is not allowed by default,
	// But we do want to allow deleting control-plane nodes from the API.
	AllowCPNode bool

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
// quorum. Use AllowQuorumLoss/AllowSingleVoter to override the dry-run safety checks instead.
func WithForceUnreachable(forceUnreachable bool) DeleteOpt {
	return func(o *DeleteOptions) {
		o.ForceUnreachable = forceUnreachable
	}
}

func WithAllowQuorumLoss(allowQuorumLoss bool) DeleteOpt {
	return func(o *DeleteOptions) {
		o.AllowQuorumLoss = allowQuorumLoss
	}
}

func WithAllowSingleVoter(allowSingleVoter bool) DeleteOpt {
	return func(o *DeleteOptions) {
		o.AllowSingleVoter = allowSingleVoter
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
	if o.AllowQuorumLoss {
		input.AllowQuorumLoss = o.AllowQuorumLoss
	}
	if o.AllowSingleVoter {
		input.AllowSingleVoter = o.AllowSingleVoter
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

	dbNode.Conditions.Deleting = db.NewCondition(true, "Node deletion in progress")

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
