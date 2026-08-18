// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
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
			// While the node is booting the boot-info RPC actually completes,
			// but returns network errors for ~2 minutes, until the node is fully booted and ready.
			// This sleep exists to rate limit the number of RPCs during this time.
			// It also smooths out the first check if it occurs before the node actually starts rebooting.
		}

		bootTime, err := a.getBootTime(ctx, input.NodeID)
		if err != nil {
			// It is normal for the boot-info RPC to return errors during the early phases of the node reboot.
			logger.Debug("Boot-info RPC returned error, this is likely normal", "nodeID", input.NodeID, "error", err)
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
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get node %s: %w", nodeID, err)
	}

	result, err := a.nodeAgentClient.GetNodeBootInfo(ctx, node)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get boot info from node %s: %w", node.Name, err)
	}
	if result.GetBootTimeUnixSeconds() <= 0 {
		return time.Time{}, fmt.Errorf("node %s returned an invalid boot time %d", node.Name, result.GetBootTimeUnixSeconds())
	}

	return time.Unix(result.GetBootTimeUnixSeconds(), 0).UTC(), nil
}
