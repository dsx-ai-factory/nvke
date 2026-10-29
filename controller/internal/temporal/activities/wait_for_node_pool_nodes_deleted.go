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

type WaitForNodePoolNodesDeletedInput struct {
	NodePoolID uuid.UUID

	NodePoolDeletingConditionMessage string
}

type WaitForNodePoolNodesDeletedResult struct{}

func (a *Activities) WaitForNodePoolNodesDeleted(
	ctx context.Context,
	input WaitForNodePoolNodesDeletedInput,
) (WaitForNodePoolNodesDeletedResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForNodePoolNodesDeletedResult]{
		Message:         input.NodePoolDeletingConditionMessage,
		UpdateCondition: a.nodePoolDeletingConditionMessageUpdater(input.NodePoolID),
		ActivityBody: func() (WaitForNodePoolNodesDeletedResult, error) {
			return a.waitForNodePoolNodesDeleted(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForNodePoolNodesDeleted(
	ctx context.Context,
	input WaitForNodePoolNodesDeletedInput,
) (WaitForNodePoolNodesDeletedResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	logger.Info("Waiting for node pool nodes to be deleted", "nodePoolID", input.NodePoolID)
	pollCtx := db.WithoutQuerySpans(ctx)

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		nodes, err := a.db.ListNodes(pollCtx, db.NodeFilters{
			NodePoolID: &input.NodePoolID,
		})
		if err != nil {
			return WaitForNodePoolNodesDeletedResult{}, fmt.Errorf("failed to list nodes: %w", err)
		}

		if len(nodes) == 0 {
			logger.Info("Node pool nodes have all been deleted", "nodePoolID", input.NodePoolID)
			return WaitForNodePoolNodesDeletedResult{}, nil
		}

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForNodePoolNodesDeletedResult{}, fmt.Errorf("context canceled while waiting for node pool nodes to be deleted: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
