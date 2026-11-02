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
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

// WaitForParentNodePoolProvisionedInput is the input for the WaitForParentNodePoolProvisioned activity.
type WaitForParentNodePoolProvisionedInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

type WaitForParentNodePoolProvisionedResult struct{}

// WaitForParentNodePoolProvisioned polls the database until the parent node pool of a node is provisioned.
func (a *Activities) WaitForParentNodePoolProvisioned(
	ctx context.Context,
	input WaitForParentNodePoolProvisionedInput,
) (WaitForParentNodePoolProvisionedResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForParentNodePoolProvisionedResult]{
		Message:         input.NodeProvisioningConditionMessage,
		UpdateCondition: a.nodeProvisioningConditionMessageUpdater(input.NodeID),
		ActivityBody: func() (WaitForParentNodePoolProvisionedResult, error) {
			return a.waitForParentNodePoolProvisioned(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForParentNodePoolProvisioned(
	ctx context.Context,
	input WaitForParentNodePoolProvisionedInput,
) (WaitForParentNodePoolProvisionedResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	// Get the node
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return WaitForParentNodePoolProvisionedResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return WaitForParentNodePoolProvisionedResult{}, fmt.Errorf("failed to get node context: %w", err)
	}
	if nodePool.ID == uuid.Nil {
		return WaitForParentNodePoolProvisionedResult{}, fmt.Errorf(
			"wait for parent node pool called for node %s without a persisted node pool",
			input.NodeID,
		)
	}

	nodePoolID := nodePool.ID
	logger.Info("Waiting for node pool to be provisioned", "nodePoolID", nodePoolID)
	pollCtx := db.WithoutQuerySpans(ctx)

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		nodePool, err := a.db.GetNodePool(pollCtx, nodePoolID)
		if err != nil {
			return WaitForParentNodePoolProvisionedResult{}, fmt.Errorf("failed to get node pool: %w", err)
		}

		// Check if the node pool is provisioned
		if nodePool.Conditions.Provisioned.Status == db.ConditionStatusTrue {
			logger.Info("Node pool is provisioned", "nodePoolID", nodePoolID)
			return WaitForParentNodePoolProvisionedResult{}, nil
		}

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForParentNodePoolProvisionedResult{},
				fmt.Errorf("context canceled while waiting for node pool provisioning: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
