// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type TriggerClusterTeardownControlPlaneNodesInput struct {
	ClusterID uuid.UUID

	ClusterDeletingConditionMessage string
}

type TriggerClusterTeardownControlPlaneNodesResult struct {
	NodeIDs []uuid.UUID
}

// TriggerClusterTeardownControlPlaneNodes marks every control-plane-controller-managed
// CP node for deletion and enqueues a TeardownNode workflow for each. These nodes
// aren't owned by a node pool, so teardownNodePools misses them.
//
// allowCPNode bypasses the quorum health gate (the whole cluster is going away, so
// the last node must be removable too); skipRemoveFromCluster skips the graceful
// drain / etcd-member removal and just deletes the infrastructure and node row.
func (a *Activities) TriggerClusterTeardownControlPlaneNodes(
	ctx context.Context,
	input TriggerClusterTeardownControlPlaneNodesInput,
) (TriggerClusterTeardownControlPlaneNodesResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[TriggerClusterTeardownControlPlaneNodesResult]{
		Message:         input.ClusterDeletingConditionMessage,
		UpdateCondition: a.clusterDeletingConditionMessageUpdater(input.ClusterID),
		ActivityBody: func() (TriggerClusterTeardownControlPlaneNodesResult, error) {
			return a.triggerClusterTeardownControlPlaneNodes(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) triggerClusterTeardownControlPlaneNodes(
	ctx context.Context,
	input TriggerClusterTeardownControlPlaneNodesInput,
) (TriggerClusterTeardownControlPlaneNodesResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Triggering teardown workflows for cluster-level control-plane nodes",
		"clusterID", input.ClusterID,
	)

	var nodeIDs []uuid.UUID
	if err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cpTrue := true
		nodes, err := tx.ListNodes(ctx, db.NodeFilters{
			ClusterID:    &input.ClusterID,
			ControlPlane: &cpTrue,
		})
		if err != nil {
			return fmt.Errorf("failed to list control-plane nodes: %w", err)
		}

		for _, node := range nodes {
			// Skip nodepool-managed CP nodes; teardownNodePools handles those.
			if !node.IsManagedByControlPlaneController() {
				continue
			}

			nodeToDelete, err := tx.GetNodeForUpdate(ctx, node.ID)
			if err != nil {
				return fmt.Errorf("failed to get control-plane node %s for update: %w", node.ID, err)
			}
			nodeToDelete.Conditions.Deleting = db.NewCondition(true, "Cluster teardown")
			if _, err := tx.UpdateNode(ctx, &nodeToDelete); err != nil {
				return fmt.Errorf("failed to mark control-plane node %s for deletion: %w", node.ID, err)
			}

			workflowID := constants.TeardownNodeWorkflowPrefix + node.ID.String()
			if _, err := tx.CreateWorkflowOutboxEntry(ctx, constants.TeardownNodeWorkflowType, workflowID, map[string]interface{}{
				"nodeID":                   node.ID,
				skipRemoveFromClusterParam: true,
				"allowCPNode":              true,
				"reason":                   "cluster teardown",
			}); err != nil {
				return fmt.Errorf("failed to create teardown workflow outbox entry for node %s: %w", node.ID, err)
			}

			nodeIDs = append(nodeIDs, node.ID)
		}

		return nil
	}); err != nil {
		return TriggerClusterTeardownControlPlaneNodesResult{}, err
	}

	return TriggerClusterTeardownControlPlaneNodesResult{
		NodeIDs: nodeIDs,
	}, nil
}
