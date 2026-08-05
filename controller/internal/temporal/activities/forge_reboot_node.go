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
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

type ForgeRebootNodeInput struct {
	NodeID uuid.UUID

	NodeRebootingConditionMessage string
}

type ForgeRebootNodeResult struct {
	RebootStartedTime time.Time
}

func (a *Activities) ForgeRebootNode(ctx context.Context, input ForgeRebootNodeInput) (ForgeRebootNodeResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ForgeRebootNodeResult]{
		Message:         input.NodeRebootingConditionMessage,
		UpdateCondition: a.nodeRebootingConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ForgeRebootNodeResult, error) {
			return a.forgeRebootNode(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) forgeRebootNode(ctx context.Context, input ForgeRebootNodeInput) (ForgeRebootNodeResult, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("Forge rebooting node", "nodeID", input.NodeID)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ForgeRebootNodeResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "NodeNotFound", err)
		}
		return ForgeRebootNodeResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		err := fmt.Errorf("node is deleting, skipping reboot %s", input.NodeID)
		return ForgeRebootNodeResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "NodeDeleting", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return ForgeRebootNodeResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	// trigger the instance reboot
	rebootStartedTime := time.Now().UTC()
	updatedInstance, err := a.forgeProvider.TriggerInstanceReboot(ctx, cluster.OrgID, node.ForgeInstanceID)
	if err != nil {
		// Handle scenario where node is not found in forge (happens if node is deleted outside of CWE)
		if errors.Is(err, forge.ErrInstanceNotFound) {
			logger.Warn("Node not found in forge, cannot continue reboot workflow, returning non-retryable error",
				"nodeID", input.NodeID,
				"error", err.Error(),
			)
			return ForgeRebootNodeResult{}, temporal.NewApplicationErrorWithOptions(
				fmt.Sprintf("failed to trigger instance reboot: %s", err.Error()),
				"InstanceNotFound",
				temporal.ApplicationErrorOptions{
					NonRetryable: true,
					Cause:        err,
					Details:      []any{"Node not found in forge, must've been deleted outside the scope of the application. cannot continue reboot workflow"},
					Category:     temporal.ApplicationErrorCategoryBenign,
				},
			)
		}
		return ForgeRebootNodeResult{}, fmt.Errorf("failed to trigger instance reboot: %w", err)
	}

	logger.Info("Forge instance reboot initiated", "nodeID", input.NodeID, "name", updatedInstance.Name, "forgeMachineID", node.ForgeMachineID)
	return ForgeRebootNodeResult{RebootStartedTime: rebootStartedTime}, nil
}
