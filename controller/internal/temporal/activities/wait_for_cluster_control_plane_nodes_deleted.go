// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

type WaitForClusterControlPlaneNodesDeletedInput struct {
	ClusterID uuid.UUID

	ClusterDeletingConditionMessage string
}

type WaitForClusterControlPlaneNodesDeletedResult struct{}

// WaitForClusterControlPlaneNodesDeleted blocks until every control-plane-controller-managed
// CP node row for the cluster has been deleted by the TeardownNode workflows that
// TriggerClusterTeardownControlPlaneNodes enqueued.
func (a *Activities) WaitForClusterControlPlaneNodesDeleted(
	ctx context.Context,
	input WaitForClusterControlPlaneNodesDeletedInput,
) (WaitForClusterControlPlaneNodesDeletedResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForClusterControlPlaneNodesDeletedResult]{
		Message:         input.ClusterDeletingConditionMessage,
		UpdateCondition: a.clusterDeletingConditionMessageUpdater(input.ClusterID),
		ActivityBody: func() (WaitForClusterControlPlaneNodesDeletedResult, error) {
			return a.waitForClusterControlPlaneNodesDeleted(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForClusterControlPlaneNodesDeleted(
	ctx context.Context,
	input WaitForClusterControlPlaneNodesDeletedInput,
) (WaitForClusterControlPlaneNodesDeletedResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	logger.Info("Waiting for cluster-level control-plane nodes to be deleted", "clusterID", input.ClusterID)
	pollCtx := db.WithoutQuerySpans(ctx)

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		cpTrue := true
		nodes, err := a.db.ListNodes(pollCtx, db.NodeFilters{
			ClusterID:    &input.ClusterID,
			ControlPlane: &cpTrue,
		})
		if err != nil {
			return WaitForClusterControlPlaneNodesDeletedResult{}, fmt.Errorf("failed to list control-plane nodes: %w", err)
		}

		remaining := 0
		for _, node := range nodes {
			if node.IsManagedByControlPlaneController() {
				remaining++
			}
		}

		if remaining == 0 {
			logger.Info("Cluster-level control-plane nodes have all been deleted", "clusterID", input.ClusterID)
			return WaitForClusterControlPlaneNodesDeletedResult{}, nil
		}

		select {
		case <-ctx.Done():
			return WaitForClusterControlPlaneNodesDeletedResult{},
				fmt.Errorf("context canceled while waiting for cluster control-plane nodes to be deleted: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
