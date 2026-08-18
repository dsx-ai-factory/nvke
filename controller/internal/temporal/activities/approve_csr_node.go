// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

type ApproveNodeCSRInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

type ApproveNodeCSRResult struct {
	Output string
}

func (a *Activities) ApproveNodeCSR(ctx context.Context, input ApproveNodeCSRInput) (ApproveNodeCSRResult, error) {
	var conditionMessage string
	var updateCondition func(ctx context.Context, message string) error

	if input.NodeProvisioningConditionMessage != "" {
		conditionMessage = input.NodeProvisioningConditionMessage
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ApproveNodeCSRResult]{
		Message:         conditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ApproveNodeCSRResult, error) {
			return a.approveNodeCSR(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) approveNodeCSR(ctx context.Context, input ApproveNodeCSRInput) (ApproveNodeCSRResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Approving Node CSR", "nodeID", input.NodeID)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ApproveNodeCSRResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return ApproveNodeCSRResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	result, err := a.ExecuteClusterRunbook(ctx, ExecuteClusterRunbookInput{
		ClusterID:   cluster.ID,
		RunbookName: "approve-latest-node-csr",
		RunbookEnv: map[string]string{
			"NODE_NAME": node.Name,
		},
	})
	if err != nil {
		return ApproveNodeCSRResult{}, fmt.Errorf("failed to approve CSR for node: %w", err)
	}

	return ApproveNodeCSRResult{
		Output: result.Output,
	}, nil
}
