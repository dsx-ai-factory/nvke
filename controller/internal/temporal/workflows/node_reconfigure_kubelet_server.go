// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	kubeVIPMaintenanceTaintValue = "kube-vip"
)

type NodeReconfigureKubeletServerInput struct {
	Server string `json:"server"`
}

type NodeReconfigureKubeletServerResult struct{}

func NodeReconfigureKubeletServer(
	ctx workflow.Context,
	nodeID uuid.UUID,
	input NodeReconfigureKubeletServerInput,
) (NodeReconfigureKubeletServerResult, error) {
	logger := workflow.GetLogger(ctx)

	logger.Info("Reconfiguring kubelet server", "nodeID", nodeID, "server", input.Server)

	taintKey := constants.TaintKeyMaintenance
	taintValue := kubeVIPMaintenanceTaintValue
	taintEffect := constants.TaintEffectNoSchedule

	if err := taintNode(ctx, nodeID, taintKey, taintValue, taintEffect); err != nil {
		return NodeReconfigureKubeletServerResult{}, fmt.Errorf("failed to taint node: %w", err)
	}

	if err := reconfigureKubeletAPIServer(ctx, nodeID, input.Server); err != nil {
		return NodeReconfigureKubeletServerResult{}, fmt.Errorf("failed to reconfigure kubelet API server: %w", err)
	}

	if err := waitForKubeletToRenewNodeLease(ctx, nodeID); err != nil {
		return NodeReconfigureKubeletServerResult{}, fmt.Errorf("failed to wait for kubelet to renew node lease: %w", err)
	}

	// Hyphen at the end of the taint effect to remove the taint
	if err := taintNode(ctx, nodeID, taintKey, taintValue, taintEffect+"-"); err != nil {
		return NodeReconfigureKubeletServerResult{}, fmt.Errorf("failed to untaint node: %w", err)
	}

	logger.Info("Reconfigured kubelet server", "nodeID", nodeID, "server", input.Server)

	return NodeReconfigureKubeletServerResult{}, nil
}

func taintNode(ctx workflow.Context, nodeID uuid.UUID, taintKey, taintValue, taintEffect string) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 3,
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.TaintNode,
		activities.TaintNodeInput{
			NodeID:      nodeID,
			TaintKey:    taintKey,
			TaintValue:  taintValue,
			TaintEffect: taintEffect,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to taint node: %w", err)
	}

	return nil
}

func reconfigureKubeletAPIServer(ctx workflow.Context, nodeID uuid.UUID, server string) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 3,
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ReconfigureKubeletAPIServer,
		activities.ReconfigureKubeletAPIServerInput{
			NodeID: nodeID,
			Server: server,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to reconfigure kubelet API server: %w", err)
	}

	return nil
}

func waitForKubeletToRenewNodeLease(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 3,
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.WaitForNodeLeaseRenewal,
		activities.WaitForNodeLeaseRenewalInput{
			NodeID: nodeID,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to wait for kubelet to renew node lease: %w", err)
	}

	return nil
}
