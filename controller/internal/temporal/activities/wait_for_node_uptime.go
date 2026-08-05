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
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

type WaitForNodeUptimeInput struct {
	NodeID                           uuid.UUID
	NodeProvisioningConditionMessage string
	After                            *time.Time
}

type WaitForNodeUptimeResult struct {
	// The time the node booted.
	BootTime time.Time
}

func (a *Activities) WaitForNodeUptime(ctx context.Context, input WaitForNodeUptimeInput) (WaitForNodeUptimeResult, error) {
	var conditionMessage string
	var updateCondition func(ctx context.Context, message string) error

	if input.NodeProvisioningConditionMessage != "" {
		conditionMessage = input.NodeProvisioningConditionMessage
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForNodeUptimeResult]{
		Message:         conditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (WaitForNodeUptimeResult, error) {
			return a.waitForNodeUptime(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForNodeUptime(ctx context.Context, input WaitForNodeUptimeInput) (WaitForNodeUptimeResult, error) {
	logger := activity.GetLogger(ctx)

	// Wait for uptime to be later than after time
	logger.Info("Waiting for node uptime confirmation", "nodeID", input.NodeID, "after", *input.After)

	// This is a long running blocking activity.
	// Start a heartbeat goroutine to prevent the activity from timing out.
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()
	sleepDuration := 1 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return WaitForNodeUptimeResult{}, fmt.Errorf("context canceled while waiting for node reboot: %w", ctx.Err())
		case <-time.After(sleepDuration):
			// While the node is booting the uptime runbook actually completes,
			// but returns network errors for ~2 minutes, until the node is fully booted and ready.
			// This sleep exists to rate limit the number of runbook executions during this time.
			// It also smooths out the first check if it occurs before the node actually starts rebooting.
		}

		bootTime, err := a.getBootTime(ctx, input.NodeID)
		if err != nil {
			// It is normal for the uptime runbook execution to return errors during the early phases of the node reboot.
			logger.Debug("Uptime runbook execution returned error, this is likely normal", "nodeID", input.NodeID, "error", err)
			continue
		}

		if input.After != nil && bootTime.Before(*input.After) {
			// Uptime check likely completed before the node started rebooting.
			logger.Debug("Node boot time occurred before desired after time", "nodeID", input.NodeID, "bootTime", bootTime, "after", *input.After)
			continue
		}
		logger.Info("Node uptime confirmed: boot time occurred after desired time", "nodeID", input.NodeID, "bootTime", bootTime, "after", *input.After)
		return WaitForNodeUptimeResult{
			BootTime: bootTime,
		}, nil
	}
}

func (a *Activities) getBootTime(ctx context.Context, nodeID uuid.UUID) (time.Time, error) {
	result, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      nodeID, // Same as node ID by convention
		RunbookName: "uptime",
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to execute uptime runbook: %w", err)
	}

	bootTime, err := a.parseBootTime(result.Output)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse boot time result: %w", err)
	}

	return bootTime, nil
}

func (a *Activities) parseBootTime(output string) (time.Time, error) {
	// Parse the Boot Time line from the uptime runbook output
	// Expected format: "Boot Time: 2025-01-16T14:30:45Z"
	bootTimeLine := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Boot Time: ") {
			bootTimeLine = strings.TrimPrefix(line, "Boot Time: ")
			break
		}
	}
	if bootTimeLine == "" {
		return time.Time{}, fmt.Errorf("failed to find Boot Time line in output: %s", output)
	}

	// Parse the ISO 8601 timestamp
	bootTime, err := time.Parse(time.RFC3339, bootTimeLine)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse Boot Time timestamp '%s': %w", bootTimeLine, err)
	}

	return bootTime.UTC(), nil
}
