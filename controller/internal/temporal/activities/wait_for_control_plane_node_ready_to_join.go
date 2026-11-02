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

type WaitForControlPlaneNodeReadyToJoinInput struct {
	ClusterID uuid.UUID

	ClusterProvisioningConditionMessage string
}

type WaitForControlPlaneNodeReadyToJoinResult struct {
	NodeID uuid.UUID
}

// WaitForControlPlaneNodeReadyToJoin polls the database until at least one control plane node is ready to join.
func (a *Activities) WaitForControlPlaneNodeReadyToJoin(
	ctx context.Context,
	input WaitForControlPlaneNodeReadyToJoinInput,
) (WaitForControlPlaneNodeReadyToJoinResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForControlPlaneNodeReadyToJoinResult]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		ActivityBody: func() (WaitForControlPlaneNodeReadyToJoinResult, error) {
			return a.waitForControlPlaneNodeReadyToJoin(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForControlPlaneNodeReadyToJoin(
	ctx context.Context,
	input WaitForControlPlaneNodeReadyToJoinInput,
) (WaitForControlPlaneNodeReadyToJoinResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	logger.Info("Waiting for a control plane node to be ready to join", "clusterID", input.ClusterID)
	pollCtx := db.WithoutQuerySpans(ctx)

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		// Get all control plane nodes for this cluster
		controlPlane := true
		nodes, err := a.db.ListNodes(pollCtx, db.NodeFilters{
			ClusterID:    &input.ClusterID,
			ControlPlane: &controlPlane,
		})
		if err != nil {
			return WaitForControlPlaneNodeReadyToJoinResult{}, fmt.Errorf("failed to list control plane nodes: %w", err)
		}

		for _, node := range nodes {
			if node.Conditions.ReadyToJoin.Status == db.ConditionStatusTrue {
				return WaitForControlPlaneNodeReadyToJoinResult{NodeID: node.ID}, nil
			}
		}

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForControlPlaneNodeReadyToJoinResult{},
				fmt.Errorf("context canceled while waiting for a control plane node to be ready to join: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
