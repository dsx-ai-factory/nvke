// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type TriggerClusterTeardownNodePoolsInput struct {
	ClusterID             uuid.UUID
	SkipRemoveFromCluster bool

	ClusterDeletingConditionMessage string
}

type TriggerClusterTeardownNodePoolsResult struct {
	NodePoolIDs []uuid.UUID
}

func (a *Activities) TriggerClusterTeardownNodePools(
	ctx context.Context,
	input TriggerClusterTeardownNodePoolsInput,
) (TriggerClusterTeardownNodePoolsResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[TriggerClusterTeardownNodePoolsResult]{
		Message:         input.ClusterDeletingConditionMessage,
		UpdateCondition: a.clusterDeletingConditionMessageUpdater(input.ClusterID),
		ActivityBody: func() (TriggerClusterTeardownNodePoolsResult, error) {
			return a.triggerClusterTeardownNodePools(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) triggerClusterTeardownNodePools(
	ctx context.Context,
	input TriggerClusterTeardownNodePoolsInput,
) (TriggerClusterTeardownNodePoolsResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Triggering teardown workflows for node pools in cluster",
		"clusterID", input.ClusterID,
		"skipRemoveFromCluster", input.SkipRemoveFromCluster,
	)

	var nodePoolIDs []uuid.UUID
	if err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		nodePools, err := tx.ListNodePools(ctx, db.NodePoolFilters{
			ClusterID: &input.ClusterID,
		})
		if err != nil {
			return fmt.Errorf("failed to get node pools by cluster ID: %w", err)
		}

		// Add workflow outbox entry to teardown each node pool
		nodePoolIDs = make([]uuid.UUID, len(nodePools))
		for i, nodePool := range nodePools {
			nodePoolIDs[i] = nodePool.ID

			workflowID := constants.TeardownNodePoolWorkflowPrefix + nodePool.ID.String()
			if _, err = tx.CreateWorkflowOutboxEntry(ctx, "TeardownNodePool", workflowID, map[string]interface{}{
				"nodePoolID":               nodePool.ID,
				skipRemoveFromClusterParam: input.SkipRemoveFromCluster,
			}); err != nil {
				return fmt.Errorf("failed to create workflow outbox entry: %w", err)
			}
		}

		return nil
	}); err != nil {
		return TriggerClusterTeardownNodePoolsResult{}, err
	}

	return TriggerClusterTeardownNodePoolsResult{
		NodePoolIDs: nodePoolIDs,
	}, nil
}
