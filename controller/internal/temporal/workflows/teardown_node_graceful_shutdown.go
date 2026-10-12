// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	k8sControlPlaneGracefulShutdownPollInterval = 10 * time.Second
)

var errControlPlaneGracefulShutdownUnsupported = errors.New("control-plane graceful shutdown unsupported")

func waitForControlPlaneGracefulShutdown(ctx workflow.Context, input TeardownNodeInput) error {
	logger := workflow.GetLogger(ctx)

	operationID := controlPlaneGracefulShutdownOperationID(ctx, input.NodeID)

	operation, err := startControlPlaneGracefulShutdown(ctx, input, operationID)
	if err != nil {
		// agent prior to v0.1.22 does not support control-plane graceful shutdown
		// so we skip the wait for control-plane graceful shutdown if the error is that it's unsupported.
		if errors.Is(err, errControlPlaneGracefulShutdownUnsupported) {
			logger.Warn("control-plane graceful shutdown unsupported, skipping", "nodeID", input.NodeID, "error", err)
			return nil
		}
		return err
	}

	return pollControlPlaneGracefulShutdown(ctx, input, operationID, operation)
}

func startControlPlaneGracefulShutdown(ctx workflow.Context, input TeardownNodeInput, operationID string) (activities.ControlPlaneGracefulShutdownOperation, error) {
	var a *activities.Activities

	operationInput := activities.StartControlPlaneGracefulShutdownInput{
		NodeID:                       input.NodeID,
		OperationID:                  operationID,
		StopKubeAPIServer:            true,
		NodeDeletingConditionMessage: "Gracefully shutting down control-plane node",
	}

	var operation activities.ControlPlaneGracefulShutdownOperation
	if err := workflow.ExecuteActivity(withControlPlaneGracefulShutdownOptions(ctx), a.StartControlPlaneGracefulShutdown, operationInput).Get(ctx, &operation); err != nil {
		if activities.IsUnsupportedControlPlaneGracefulShutdownRPC(err) {
			workflow.GetLogger(ctx).Info("Node-agent does not support control-plane graceful shutdown RPCs; skipping", "nodeID", input.NodeID, "error", err)
			return activities.ControlPlaneGracefulShutdownOperation{}, errControlPlaneGracefulShutdownUnsupported
		}
		return activities.ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("failed to start control-plane graceful shutdown: %w", err)
	}

	if err := validateControlPlaneGracefulShutdownOperation(operationID, operation); err != nil {
		return activities.ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("invalid control-plane graceful shutdown start status: %w", err)
	}

	return operation, nil
}

func pollControlPlaneGracefulShutdown(ctx workflow.Context, input TeardownNodeInput, operationID string, operation activities.ControlPlaneGracefulShutdownOperation) error {
	logger := workflow.GetLogger(ctx)

	for operation.Phase == activities.ControlPlaneGracefulShutdownPhaseRunning {
		logger.Info("Waiting for control-plane graceful shutdown", "nodeID", input.NodeID, "operationID", operationID, "phase", operation.Phase, "step", operation.Step)

		if err := workflow.Sleep(ctx, k8sControlPlaneGracefulShutdownPollInterval); err != nil {
			return err
		}

		nextOperation, err := getControlPlaneGracefulShutdownStatus(ctx, input, operationID)
		if err != nil {
			return err
		}
		operation = nextOperation
	}

	switch operation.Phase {
	case activities.ControlPlaneGracefulShutdownPhaseSucceeded:
		return nil
	case activities.ControlPlaneGracefulShutdownPhaseFailed:
		return fmt.Errorf("control-plane graceful shutdown failed at %s: %s", operation.Step, operation.Error)
	default:
		return fmt.Errorf("control-plane graceful shutdown returned unknown phase %q", operation.Phase)
	}
}

func getControlPlaneGracefulShutdownStatus(ctx workflow.Context, input TeardownNodeInput, operationID string) (activities.ControlPlaneGracefulShutdownOperation, error) {
	var a *activities.Activities

	statusInput := activities.GetControlPlaneGracefulShutdownStatusInput{
		NodeID:                       input.NodeID,
		OperationID:                  operationID,
		NodeDeletingConditionMessage: "Waiting for control-plane graceful shutdown",
	}

	var operation activities.ControlPlaneGracefulShutdownOperation
	if err := workflow.ExecuteActivity(withControlPlaneGracefulShutdownOptions(ctx), a.GetControlPlaneGracefulShutdownStatus, statusInput).Get(ctx, &operation); err != nil {
		return activities.ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("failed to poll control-plane graceful shutdown status: %w", err)
	}

	if err := validateControlPlaneGracefulShutdownOperation(operationID, operation); err != nil {
		return activities.ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("invalid control-plane graceful shutdown status: %w", err)
	}

	return operation, nil
}

func validateControlPlaneGracefulShutdownOperation(operationID string, operation activities.ControlPlaneGracefulShutdownOperation) error {
	if operation.ID == "" {
		return errors.New("agent returned empty operation ID")
	}
	if operation.ID != operationID {
		return fmt.Errorf("agent returned operation ID %q, expected %q", operation.ID, operationID)
	}

	switch operation.Phase {
	case activities.ControlPlaneGracefulShutdownPhaseRunning,
		activities.ControlPlaneGracefulShutdownPhaseSucceeded,
		activities.ControlPlaneGracefulShutdownPhaseFailed:
		return nil
	case "", activities.ControlPlaneGracefulShutdownPhaseUnspecified:
		return fmt.Errorf("agent returned unspecified operation phase %q", operation.Phase)
	default:
		return fmt.Errorf("agent returned unknown operation phase %q", operation.Phase)
	}
}

func withControlPlaneGracefulShutdownOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    1 * time.Minute,
		ScheduleToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 2 * time.Second,
			MaximumInterval: 10 * time.Second,
		},
	})
}

func controlPlaneGracefulShutdownOperationID(ctx workflow.Context, nodeID uuid.UUID) string {
	info := workflow.GetInfo(ctx)
	return fmt.Sprintf("%s/%s/k8s-control-plane-node-graceful-shutdown", info.WorkflowExecution.ID, nodeID.String())
}
