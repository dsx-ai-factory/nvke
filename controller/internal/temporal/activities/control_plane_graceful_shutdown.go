// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	UnsupportedControlPlaneGracefulShutdownRPCErrorType = "UnsupportedK8sControlPlaneNodeGracefulShutdownRPC"

	ControlPlaneGracefulShutdownPhaseUnspecified = "unspecified"
	ControlPlaneGracefulShutdownPhaseRunning     = "running"
	ControlPlaneGracefulShutdownPhaseSucceeded   = "succeeded"
	ControlPlaneGracefulShutdownPhaseFailed      = "failed"
)

type ControlPlaneGracefulShutdownOperation struct {
	ID    string
	Phase string
	Step  string
	Error string
}

type StartControlPlaneGracefulShutdownInput struct {
	NodeID            uuid.UUID
	OperationID       string
	StopKubeAPIServer bool

	NodeDeletingConditionMessage string
}

type GetControlPlaneGracefulShutdownStatusInput struct {
	NodeID      uuid.UUID
	OperationID string

	NodeDeletingConditionMessage string
}

func (a *Activities) StartControlPlaneGracefulShutdown(
	ctx context.Context,
	input StartControlPlaneGracefulShutdownInput,
) (ControlPlaneGracefulShutdownOperation, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ControlPlaneGracefulShutdownOperation]{
		Message:         input.NodeDeletingConditionMessage,
		UpdateCondition: a.nodeDeletingConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ControlPlaneGracefulShutdownOperation, error) {
			return a.startControlPlaneGracefulShutdown(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) startControlPlaneGracefulShutdown(
	ctx context.Context,
	input StartControlPlaneGracefulShutdownInput,
) (ControlPlaneGracefulShutdownOperation, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("failed to get node: %w", err)
	}

	resp, err := a.nodeAgentClient.StartK8sControlPlaneNodeGracefulShutdown(ctx, node, &nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest{
		OperationId:       input.OperationID,
		StopKubeApiserver: input.StopKubeAPIServer,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return ControlPlaneGracefulShutdownOperation{}, newUnsupportedControlPlaneGracefulShutdownRPCError(node.Name, err)
		}
		return ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("failed to start control-plane graceful shutdown on node %s: %w", node.Name, err)
	}

	return controlPlaneGracefulShutdownOperationFromProto(resp.GetOperation()), nil
}

func (a *Activities) GetControlPlaneGracefulShutdownStatus(
	ctx context.Context,
	input GetControlPlaneGracefulShutdownStatusInput,
) (ControlPlaneGracefulShutdownOperation, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ControlPlaneGracefulShutdownOperation]{
		Message:         input.NodeDeletingConditionMessage,
		UpdateCondition: a.nodeDeletingConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ControlPlaneGracefulShutdownOperation, error) {
			return a.getControlPlaneGracefulShutdownStatus(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) getControlPlaneGracefulShutdownStatus(
	ctx context.Context,
	input GetControlPlaneGracefulShutdownStatusInput,
) (ControlPlaneGracefulShutdownOperation, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("failed to get node: %w", err)
	}

	resp, err := a.nodeAgentClient.GetK8sControlPlaneNodeGracefulShutdownStatus(ctx, node, &nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest{
		OperationId: input.OperationID,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return ControlPlaneGracefulShutdownOperation{}, newUnsupportedControlPlaneGracefulShutdownRPCError(node.Name, err)
		}
		return ControlPlaneGracefulShutdownOperation{}, fmt.Errorf("failed to get control-plane graceful shutdown status from node %s: %w", node.Name, err)
	}

	return controlPlaneGracefulShutdownOperationFromProto(resp.GetOperation()), nil
}

func IsUnsupportedControlPlaneGracefulShutdownRPC(err error) bool {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Type() == UnsupportedControlPlaneGracefulShutdownRPCErrorType
	}
	return status.Code(err) == codes.Unimplemented
}

func newUnsupportedControlPlaneGracefulShutdownRPCError(nodeName string, err error) error {
	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf("agent on node %s does not support control-plane graceful shutdown RPCs", nodeName),
		UnsupportedControlPlaneGracefulShutdownRPCErrorType,
		err,
	)
}

func controlPlaneGracefulShutdownOperationFromProto(operation *nvkeproto.NodeAgentOperationStatus) ControlPlaneGracefulShutdownOperation {
	if operation == nil {
		return ControlPlaneGracefulShutdownOperation{}
	}
	return ControlPlaneGracefulShutdownOperation{
		ID:    operation.GetOperationId(),
		Phase: controlPlaneGracefulShutdownPhase(operation.GetPhase()),
		Step:  operation.GetStep(),
		Error: operation.GetErrorMessage(),
	}
}

func controlPlaneGracefulShutdownPhase(phase nvkeproto.NodeAgentOperationPhase) string {
	switch phase {
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
		return ControlPlaneGracefulShutdownPhaseUnspecified
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
		return ControlPlaneGracefulShutdownPhaseRunning
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
		return ControlPlaneGracefulShutdownPhaseSucceeded
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		return ControlPlaneGracefulShutdownPhaseFailed
	default:
		return ControlPlaneGracefulShutdownPhaseUnspecified
	}
}
