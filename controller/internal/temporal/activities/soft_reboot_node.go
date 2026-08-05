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

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

type SoftRebootNodeInput struct {
	NodeID uuid.UUID

	NodeRebootingConditionMessage string
}

type SoftRebootNodeResult struct {
	Output string
}

func (a *Activities) SoftRebootNode(ctx context.Context, input SoftRebootNodeInput) (SoftRebootNodeResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[SoftRebootNodeResult]{
		Message:         input.NodeRebootingConditionMessage,
		UpdateCondition: a.nodeRebootingConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (SoftRebootNodeResult, error) {
			return a.softRebootNode(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) softRebootNode(ctx context.Context, input SoftRebootNodeInput) (SoftRebootNodeResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Soft rebooting node", "nodeID", input.NodeID)

	// The reboot runbook triggers reboot in background and exits immediately without waiting for the reboot to complete.
	_, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      input.NodeID,
		RunbookName: "reboot",
	})
	if err != nil {
		return SoftRebootNodeResult{}, fmt.Errorf("failed to execute reboot runbook: %w", err)
	}

	return SoftRebootNodeResult{}, nil
}
