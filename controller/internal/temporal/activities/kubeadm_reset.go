// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

type KubeadmResetInput struct {
	NodeID uuid.UUID

	SkipPhases []string

	NodeDeletingConditionMessage string
}

// KubeadmResetResult is intentionally empty. node-agent runs kubeadm reset
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
