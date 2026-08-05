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
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/NVIDIA/nke/controller/internal/db"
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

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		// Get the cluster
		cluster, err := a.db.GetCluster(ctx, nodePool.ClusterID)
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
