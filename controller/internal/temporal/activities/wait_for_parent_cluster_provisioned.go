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

// WaitForParentClusterProvisionedInput is the input for the WaitForParentClusterProvisioned activity.
type WaitForParentClusterProvisionedInput struct {
	NodePoolID uuid.UUID

	NodePoolProvisioningConditionMessage string
}

type WaitForParentClusterProvisionedResult struct{}

// WaitForParentClusterProvisioned polls the database until a cluster is provisioned.
func (a *Activities) WaitForParentClusterProvisioned(
	ctx context.Context,
	input WaitForParentClusterProvisionedInput,
) (WaitForParentClusterProvisionedResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForParentClusterProvisionedResult]{
		Message:         input.NodePoolProvisioningConditionMessage,
		UpdateCondition: a.nodePoolProvisioningConditionMessageUpdater(input.NodePoolID),
		ActivityBody: func() (WaitForParentClusterProvisionedResult, error) {
			return a.waitForParentClusterProvisioned(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForParentClusterProvisioned(
	ctx context.Context,
	input WaitForParentClusterProvisionedInput,
) (WaitForParentClusterProvisionedResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	// Get the node pool
	nodePool, err := a.db.GetNodePool(ctx, input.NodePoolID)
	if err != nil {
		return WaitForParentClusterProvisionedResult{}, fmt.Errorf("failed to get node pool: %w", err)
	}

	logger.Info("Waiting for cluster to be provisioned", "clusterID", nodePool.ClusterID)
	pollCtx := db.WithoutQuerySpans(ctx)

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		// Get the cluster
		cluster, err := a.db.GetCluster(pollCtx, nodePool.ClusterID)
		if err != nil {
			return WaitForParentClusterProvisionedResult{}, fmt.Errorf("failed to get cluster: %w", err)
		}

		// Check if the cluster is provisioned
		if cluster.Conditions.Provisioned.Status == db.ConditionStatusTrue {
			logger.Info("Cluster is provisioned", "clusterID", nodePool.ClusterID)
			return WaitForParentClusterProvisionedResult{}, nil
		}

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForParentClusterProvisionedResult{}, fmt.Errorf("context canceled while waiting for cluster provisioning: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
