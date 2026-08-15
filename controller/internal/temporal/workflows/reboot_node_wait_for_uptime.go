// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type RebootNodeWaitForUptimeInput struct {
	NodeID  uuid.UUID
	Timeout time.Duration
}

func RebootNodeWaitForUptime(ctx workflow.Context, input RebootNodeWaitForUptimeInput) error {
	logger := workflow.GetLogger(ctx)

	nodeID := input.NodeID
	info := workflow.GetInfo(ctx)
	rebootAttempt := int(info.Attempt)

	logger.Info("Soft Reboot Node", "rebootAttempt", rebootAttempt)

	// Get node Info from DB using nodeID
	var a *activities.Activities
	node := db.Node{}
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, nodeID).Get(ctx, &node); err != nil {
		return fmt.Errorf("failed to get node info for node: %w", err)
	}
	defer func() {
		// Clean up if the workflow is canceled
		if node.Name != "" {
			metrics.ClearCurrentAttempt(node.Name)
		}
	}()

	// Set current attempt metric
	if !workflow.IsReplaying(ctx) {
		metrics.SetCurrentAttempt(node.Name, rebootAttempt)
	}

	// capture current logical time within a Temporal workflow  to compare with node boot time.
	// Must be captured BEFORE the reboot call so that a very fast restart (e.g. Docker container
	// restarts in <1s) doesn't produce a boot time earlier than rebootInitiatedTime.
	rebootInitiatedTime := workflow.Now(ctx)

	// Trigger a node-local reboot and wait for the node uptime to advance.
	// maxAttempts is 0 because we want to retry indefinitely until the node is reachable.
	if err := softRebootNode(ctx, input.NodeID, rebootAttempt, 0); err != nil {
		return fmt.Errorf("failed to reboot node: %w", err)
	}

	// update reboot msg to indicate forge reboot is initiated and we'll wait for uptime
	if err := updateNodeRebootingCondition(ctx, nodeID, db.ConditionStatusTrue, fmt.Sprintf("rebootAttempt: %d - soft reboot initiated. Wait for node uptime", rebootAttempt)); err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}
	logger.Info("Wait for uptime after node reboot", "rebootAttempt", rebootAttempt)

	upTimeResult, err := waitForNodeUpTime(ctx, nodeID, rebootInitiatedTime, input.Timeout)
	if err != nil {
		return fmt.Errorf("failed to get node uptime: %w", err)
	}
	// Success. Update reboot msg to indicate forge reboot is successful
	if err := updateNodeRebootingCondition(ctx, nodeID, db.ConditionStatusFalse, fmt.Sprintf("rebootAttempt: %d - Node successfully rebooted at: %s", rebootAttempt, upTimeResult.BootTime)); err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}
	logger.Info("Successfully rebooted and uptime confirmed", "rebootAttempt", rebootAttempt, "rebootInitiated", rebootInitiatedTime, "nodeBootTime", upTimeResult.BootTime)

	// Update metrics for successful completion
	rebootReason := constants.RebootNodeReasonProvisioningWorkflow
	duration := workflow.Now(ctx).Sub(rebootInitiatedTime)
	if !workflow.IsReplaying(ctx) {
		metrics.RecordRebootCompleted(rebootReason, duration, rebootAttempt)
	}
	return nil
}

func softRebootNode(ctx workflow.Context, nodeID uuid.UUID, rebootAttempt int, maxAttempts int32) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 6 * time.Minute, // reboot runbook may wait up to 5m for apt/dpkg locks before issuing reboot.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    30 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    5 * time.Minute,
			MaximumAttempts:    maxAttempts,
		},
	}

	return workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.SoftRebootNode,
		activities.SoftRebootNodeInput{
			NodeID:                        nodeID,
			NodeRebootingConditionMessage: fmt.Sprintf("Soft rebooting node (attempt: %d)", rebootAttempt),
		},
	).Get(ctx, nil)
}

func waitForNodeUpTime(ctx workflow.Context, nodeID uuid.UUID, after time.Time, timeout time.Duration) (activities.WaitForNodeUptimeResult, error) {
	var a *activities.Activities
	info := workflow.GetInfo(ctx)
	rebootAttempt := int(info.Attempt)

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 1,
			MaximumAttempts:    1,
		},
	}
	var upTimeResult activities.WaitForNodeUptimeResult

	// Wait for uptime activity
	err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.WaitForNodeUptime,
		activities.WaitForNodeUptimeInput{
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: fmt.Sprintf("rebootAttempt: %d - Waiting for uptime", rebootAttempt),
			After:                            &after,
		},
	).Get(ctx, &upTimeResult)

	return upTimeResult, err
}
