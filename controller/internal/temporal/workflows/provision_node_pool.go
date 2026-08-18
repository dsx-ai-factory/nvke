// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type ProvisionNodePoolInput struct {
	NodePoolID uuid.UUID
}

func ProvisionNodePool(ctx workflow.Context, input ProvisionNodePoolInput) error {
	logger := workflow.GetLogger(ctx)

	if err := provisionNodePool(ctx, input); err != nil {
		// Best effort to update the node pool conditions to indicate the provisioning workflow failed
		if err := updateNodePoolProvisioningMessage(ctx, input.NodePoolID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node pool conditions", "error", err)
		}

		return fmt.Errorf("failed to provision node pool: %w", err)
	}

	return nil
}

func provisionNodePool(ctx workflow.Context, input ProvisionNodePoolInput) error {
	if input.NodePoolID == uuid.Nil {
		return errors.New("nodePoolID is required")
	}

	if err := waitForClusterProvisioning(ctx, input.NodePoolID); err != nil {
		return fmt.Errorf("failed to wait for cluster provisioning: %w", err)
	}

	if err := markNodePoolAsProvisioned(ctx, input.NodePoolID); err != nil {
		return fmt.Errorf("failed to mark node pool as provisioned: %w", err)
	}

	return nil
}

func waitForClusterProvisioning(ctx workflow.Context, nodePoolID uuid.UUID) error {
	var a *activities.Activities

	waitOptions := workflow.ActivityOptions{
		// Takes longest on initial cluster provisioning while waiting for the cluster to be ready.
		// If adding a new node pool to an existing cluster, this should return immediately
		StartToCloseTimeout: 1 * time.Hour,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely, giving operator time to manually resolve any issues
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.WaitForParentClusterProvisioned,
		activities.WaitForParentClusterProvisionedInput{
			NodePoolID:                           nodePoolID,
			NodePoolProvisioningConditionMessage: "Waiting for cluster provisioning",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to wait for cluster provisioning: %w", err)
	}

	return nil
}

func updateNodePoolProvisioningMessage(ctx workflow.Context, nodePoolID uuid.UUID, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodePoolConditions, activities.NodePoolConditionsUpdate{
		NodePoolID: nodePoolID,
		Conditions: activities.NodePoolConditions{
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	}).Get(ctx, nil)
}

func markNodePoolAsProvisioned(ctx workflow.Context, nodePoolID uuid.UUID) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodePoolConditions, activities.NodePoolConditionsUpdate{
		NodePoolID: nodePoolID,
		Conditions: activities.NodePoolConditions{
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusFalse,
				Message: provisioningCompleted,
			},
			Provisioned: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Provisioned node pool",
			},
		},
	}).Get(ctx, nil)
}
