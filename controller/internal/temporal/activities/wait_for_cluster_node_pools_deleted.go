// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type WaitForClusterNodePoolsDeletedInput struct {
	ClusterID uuid.UUID

	ClusterDeletingConditionMessage string
}

type WaitForClusterNodePoolsDeletedResult struct{}

func (a *Activities) WaitForClusterNodePoolsDeleted(
	ctx context.Context,
	input WaitForClusterNodePoolsDeletedInput,
) (WaitForClusterNodePoolsDeletedResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForClusterNodePoolsDeletedResult]{
		Message:         input.ClusterDeletingConditionMessage,
		UpdateCondition: a.clusterDeletingConditionMessageUpdater(input.ClusterID),
		ActivityBody: func() (WaitForClusterNodePoolsDeletedResult, error) {
			return a.waitForClusterNodePoolsDeleted(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForClusterNodePoolsDeleted(
	ctx context.Context,
	input WaitForClusterNodePoolsDeletedInput,
) (WaitForClusterNodePoolsDeletedResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	logger.Info("Waiting for cluster node pools to be deleted", "clusterID", input.ClusterID)
	pollCtx := db.WithoutQuerySpans(ctx)

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		nodePools, err := a.db.ListNodePools(pollCtx, db.NodePoolFilters{
			ClusterID: &input.ClusterID,
		})
		if err != nil {
			return WaitForClusterNodePoolsDeletedResult{}, fmt.Errorf("failed to list node pools: %w", err)
		}

		if len(nodePools) == 0 {
			logger.Info("Cluster node pools have all been deleted", "clusterID", input.ClusterID)
			return WaitForClusterNodePoolsDeletedResult{}, nil
		}

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForClusterNodePoolsDeletedResult{},
				fmt.Errorf("context canceled while waiting for cluster node pools to be deleted: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
