// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return SoftRebootNodeResult{}, fmt.Errorf("failed to get node %s: %w", input.NodeID, err)
	}

	// The agent starts reboot as a detached process, so this call does not wait
	// for the reboot to complete or require the connection to survive it.
	response, err := a.nodeAgentClient.ScheduleNodeReboot(ctx, node)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			logger.Info("ScheduleNodeReboot is unavailable; falling back to legacy reboot runbook", "nodeID", node.ID, "nodeName", node.Name)
			if _, runbookErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
				HostID:      input.NodeID,
				RunbookName: "reboot",
			}); runbookErr != nil {
				return SoftRebootNodeResult{}, fmt.Errorf("failed to execute legacy reboot runbook on node %s: %w", node.Name, runbookErr)
			}
			return SoftRebootNodeResult{}, nil
		}
		return SoftRebootNodeResult{}, fmt.Errorf("failed to schedule soft reboot on node %s: %w", node.Name, err)
	}
	if response.GetPackageLocksTimedOut() {
		logger.Warn("Package-manager lock wait timed out; reboot was scheduled", "nodeID", node.ID, "nodeName", node.Name)
	}

	return SoftRebootNodeResult{}, nil
}
