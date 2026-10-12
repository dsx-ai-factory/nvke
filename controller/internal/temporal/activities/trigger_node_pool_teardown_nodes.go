// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type TriggerNodePoolTeardownNodesInput struct {
	NodePoolID            uuid.UUID
	SkipRemoveFromCluster bool

	NodePoolDeletingConditionMessage string
}

type TriggerNodePoolTeardownNodesResult struct {
	NodeIDs []uuid.UUID
}

func (a *Activities) TriggerNodePoolTeardownNodes(
	ctx context.Context,
	input TriggerNodePoolTeardownNodesInput,
) (TriggerNodePoolTeardownNodesResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[TriggerNodePoolTeardownNodesResult]{
		Message:         input.NodePoolDeletingConditionMessage,
		UpdateCondition: a.nodePoolDeletingConditionMessageUpdater(input.NodePoolID),
		ActivityBody: func() (TriggerNodePoolTeardownNodesResult, error) {
			return a.triggerNodePoolTeardownNodes(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) triggerNodePoolTeardownNodes(
	ctx context.Context,
	input TriggerNodePoolTeardownNodesInput,
) (TriggerNodePoolTeardownNodesResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Triggering teardown workflows for nodes in node pool",
		"nodePoolID", input.NodePoolID,
		"skipRemoveFromCluster", input.SkipRemoveFromCluster,
	)

	var nodeIDs []uuid.UUID
	if err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Get the list of nodes in the node pool
		nodes, err := tx.ListNodes(ctx, db.NodeFilters{
			NodePoolID: &input.NodePoolID,
		})
		if err != nil {
			return fmt.Errorf("failed to get nodes by node pool ID: %w", err)
		}

		// Add workflow outbox entry to teardown each node
		for _, node := range nodes {
			workflowID := constants.TeardownNodeWorkflowPrefix + node.ID.String()
			if _, err = tx.CreateWorkflowOutboxEntry(ctx, constants.TeardownNodeWorkflowType, workflowID, map[string]interface{}{
				"nodeID":                   node.ID,
				skipRemoveFromClusterParam: input.SkipRemoveFromCluster,
				"allowCPNode":              true,
			}); err != nil {
				return fmt.Errorf("failed to create workflow outbox entry: %w", err)
			}
		}

		return nil
	}); err != nil {
		return TriggerNodePoolTeardownNodesResult{}, err
	}

	return TriggerNodePoolTeardownNodesResult{
		NodeIDs: nodeIDs,
	}, nil
}
