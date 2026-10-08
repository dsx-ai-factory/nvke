// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	legacyRebootRunbookName = "reboot"
)

type SoftRebootNodeInput struct {
	NodeID              uuid.UUID
	RebootInitiatedTime time.Time

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

	if activity.GetInfo(ctx).Attempt > 1 && !input.RebootInitiatedTime.IsZero() {
		bootTime, err := a.getBootTime(ctx, input.NodeID)
		if err != nil {
			return SoftRebootNodeResult{}, fmt.Errorf("failed to determine whether node %s already rebooted: %w", input.NodeID, err)
		}
		if !bootTime.Before(input.RebootInitiatedTime) {
			logger.Info("Node already rebooted; skipping duplicate reboot request",
				"nodeID", input.NodeID,
				"bootTime", bootTime,
				"rebootInitiatedTime", input.RebootInitiatedTime,
			)
			return SoftRebootNodeResult{}, nil
		}
	}

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return SoftRebootNodeResult{}, fmt.Errorf("failed to get node %s: %w", input.NodeID, err)
	}
	if node.ControlPlane {
		if err := a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
			NodeID: node.ID, Conditions: NodeConditions{Rebooting: &ConditionUpdate{
				Status: db.ConditionStatusTrue, Message: "Control-plane reboot is in progress",
			}},
		}); err != nil {
			return SoftRebootNodeResult{}, err
		}
	}

	// The agent starts reboot as a detached process, so this call does not wait
	// for the reboot to complete or require the connection to survive it.
	response, err := a.nodeAgentClient.ScheduleNodeReboot(ctx, node)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			logger.Info("ScheduleNodeReboot is unavailable; falling back to legacy reboot runbook", "nodeID", node.ID, "nodeName", node.Name)
			if _, runbookErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
				HostID:      input.NodeID,
				RunbookName: legacyRebootRunbookName,
			}); runbookErr != nil {
				if isRebootSchedulingOutcomeUnknown(runbookErr) {
					logger.Warn("Legacy reboot scheduling outcome is unknown; deferring to boot-state verification",
						"nodeID", node.ID,
						"nodeName", node.Name,
						"error", runbookErr,
					)
					return SoftRebootNodeResult{}, nil
				}
				return SoftRebootNodeResult{}, fmt.Errorf("failed to execute legacy reboot runbook on node %s: %w", node.Name, runbookErr)
			}
			return SoftRebootNodeResult{}, nil
		}
		if isRebootSchedulingOutcomeUnknown(err) {
			logger.Warn("Soft reboot scheduling outcome is unknown; deferring to boot-state verification",
				"nodeID", node.ID,
				"nodeName", node.Name,
				"error", err,
			)
			return SoftRebootNodeResult{}, nil
		}
		return SoftRebootNodeResult{}, fmt.Errorf("failed to schedule soft reboot on node %s: %w", node.Name, err)
	}
	if response.GetPackageLocksTimedOut() {
		logger.Warn("Package-manager lock wait timed out; reboot was scheduled", "nodeID", node.ID, "nodeName", node.Name)
	}

	return SoftRebootNodeResult{}, nil
}

func isRebootSchedulingOutcomeUnknown(err error) bool {
	code := status.Code(err)
	return code == codes.Unavailable || code == codes.DeadlineExceeded
}
