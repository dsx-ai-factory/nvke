// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type TeardownNodePoolInput struct {
	NodePoolID            uuid.UUID
	SkipRemoveFromCluster bool
}

func TeardownNodePool(ctx workflow.Context, input TeardownNodePoolInput) error {
	logger := workflow.GetLogger(ctx)

	if err := teardownNodePool(ctx, input); err != nil {
		// Best effort to update the node pool conditions to indicate the teardown workflow failed
		if err := updateNodePoolDeletingMessage(ctx, input.NodePoolID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node pool conditions", "error", err)
		}

		return fmt.Errorf("failed to teardown node pool: %w", err)
	}

	return nil
}

func teardownNodePool(ctx workflow.Context, input TeardownNodePoolInput) error {
	var a *activities.Activities

	if input.NodePoolID == uuid.Nil {
		return errors.New("nodePoolID is required")
	}

	if err := cancelNodePoolProvisionWorkflow(ctx, input.NodePoolID); err != nil {
		return fmt.Errorf("failed to cancel node pool provision workflow: %w", err)
	}

	if err := teardownNodes(ctx, input.NodePoolID, input.SkipRemoveFromCluster); err != nil {
		return fmt.Errorf("failed to teardown nodes: %w", err)
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.DeleteNodePool, input.NodePoolID).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete node pool: %w", err)
	}

	return nil
}

func cancelNodePoolProvisionWorkflow(ctx workflow.Context, nodePoolID uuid.UUID) error {
	if err := updateNodePoolDeletingMessage(ctx, nodePoolID, "Canceling provision workflow"); err != nil {
		return fmt.Errorf("failed to update node pool conditions: %w", err)
	}

	// Cancel the nodePool provision workflow if it exists
	provisionWorkflowID := fmt.Sprintf("%s%s", constants.ProvisionNodePoolWorkflowPrefix, nodePoolID)
	if err := workflow.RequestCancelExternalWorkflow(ctx, provisionWorkflowID, "").Get(ctx, nil); err != nil {
		var notFound *temporal.UnknownExternalWorkflowExecutionError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to cancel provision workflow: %w", err)
		}
		// Workflow doesn't exist, which we'll consider to be a successful cancellation
		return nil
	}

	return nil
}

func teardownNodes(ctx workflow.Context, nodePoolID uuid.UUID, skipRemoveFromCluster bool) error {
	var a *activities.Activities

	// Trigger teardown of all nodes via workflow outbox
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.TriggerNodePoolTeardownNodes,
		activities.TriggerNodePoolTeardownNodesInput{
			NodePoolID:                       nodePoolID,
			SkipRemoveFromCluster:            skipRemoveFromCluster,
			NodePoolDeletingConditionMessage: "Triggering teardown of nodes",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to trigger teardown of node pool nodes: %w", err)
	}

	// Monitor the database until all nodes are deleted
	waitOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumInterval: time.Minute,
			MaximumAttempts: 0,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.WaitForNodePoolNodesDeleted,
		activities.WaitForNodePoolNodesDeletedInput{
			NodePoolID:                       nodePoolID,
			NodePoolDeletingConditionMessage: "Waiting for nodes to be deleted",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to monitor node pool deletion: %w", err)
	}

	return nil
}

func updateNodePoolDeletingMessage(ctx workflow.Context, nodePoolID uuid.UUID, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodePoolConditions, activities.NodePoolConditionsUpdate{
		NodePoolID: nodePoolID,
		Conditions: activities.NodePoolConditions{
			Deleting: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	}).Get(ctx, nil)
}
