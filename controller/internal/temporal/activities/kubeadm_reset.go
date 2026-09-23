// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

type KubeadmResetInput struct {
	NodeID uuid.UUID

	SkipPhases []string

	NodeDeletingConditionMessage string
}

// KubeadmResetResult is intentionally empty. agent runs kubeadm reset
// synchronously and always returns an empty KubeadmResetStatusResponse; outcome
// is written to local NodeStatus, not the RPC body. SWE's node_agent_reset used
// an empty Response struct for the same reason. Success is nil error; failure is
// RPC/connection errors only (kubeadm exit failures are not surfaced as gRPC errors).
type KubeadmResetResult struct{}

func (a *Activities) KubeadmReset(ctx context.Context, input KubeadmResetInput) (KubeadmResetResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[KubeadmResetResult]{
		Message:         input.NodeDeletingConditionMessage,
		UpdateCondition: a.nodeDeletingConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (KubeadmResetResult, error) {
			return a.kubeadmReset(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) kubeadmReset(ctx context.Context, input KubeadmResetInput) (KubeadmResetResult, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return KubeadmResetResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	_, err = a.nodeAgentClient.ExecuteKubeadmReset(ctx, node, &nkeproto.KubeadmResetRequest{
		SkipPhases: input.SkipPhases,
	})
	if err != nil {
		return KubeadmResetResult{}, fmt.Errorf("failed to execute kubeadm reset on node %s: %w", node.Name, err)
	}

	return KubeadmResetResult{}, nil
}
