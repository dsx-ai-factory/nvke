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

	"github.com/dsx-ai-factory/nke/controller/internal/temporal/activities"

	corev1 "k8s.io/api/core/v1"
)

const (
	ApplyK8sNodeConditionWorkflowName = "ApplyK8sNodeCondition"
)

type ApplyK8sNodeConditionInput struct {
	NodeID        uuid.UUID
	NodeCondition corev1.NodeCondition
}

// ApplyK8sNodeCondition applies a corev1.NodeCondition to a kubernetes node.
// Not to be confused with controller db.NodeCondition.
func ApplyK8sNodeCondition(ctx workflow.Context, input ApplyK8sNodeConditionInput) error {
	logger := workflow.GetLogger(ctx)

	logger.Info("Applying node condition", "nodeID", input.NodeID, "nodeCondition", input.NodeCondition)

	if input.NodeID == uuid.Nil {
		return errors.New("nodeID is required")
	}

	if err := applyK8sNodeCondition(ctx, input.NodeID, input.NodeCondition); err != nil {
		return fmt.Errorf("failed to apply node condition: %w", err)
	}

	logger.Info("Successfully applied node condition", "nodeID", input.NodeID, "nodeCondition", input.NodeCondition)
	return nil
}

func applyK8sNodeCondition(ctx workflow.Context, nodeID uuid.UUID, nodeCondition corev1.NodeCondition) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ApplyNodeCondition,
		activities.ApplyNodeConditionInput{
			NodeID:        nodeID,
			NodeCondition: nodeCondition,
		},
	).Get(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to apply node condition: %w", err)
	}

	return nil
}
