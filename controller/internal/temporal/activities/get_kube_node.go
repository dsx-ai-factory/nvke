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

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type GetKubeNodeInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
	NodeRebootingConditionMessage    string
}

type GetKubeNodeResult struct {
	NodeID uuid.UUID
	Node   corev1.Node
}

func (a *Activities) GetKubeNode(ctx context.Context, input GetKubeNodeInput) (GetKubeNodeResult, error) {
	var conditionMessage string
	var updateCondition func(ctx context.Context, message string) error

	if input.NodeProvisioningConditionMessage != "" {
		conditionMessage = input.NodeProvisioningConditionMessage
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	} else if input.NodeRebootingConditionMessage != "" {
		conditionMessage = input.NodeRebootingConditionMessage
		updateCondition = a.nodeRebootingConditionMessageUpdater(input.NodeID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[GetKubeNodeResult]{
		Message:         conditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (GetKubeNodeResult, error) {
			return a.getKubeNode(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) getKubeNode(ctx context.Context, input GetKubeNodeInput) (GetKubeNodeResult, error) {
	logger := activity.GetLogger(ctx)
	logger.Debug("Get Kube Node", "nodeID", input.NodeID)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return GetKubeNodeResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return GetKubeNodeResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return GetKubeNodeResult{}, err
	}

	var kubeNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: node.Name}, &kubeNode); err != nil {
		return GetKubeNodeResult{}, fmt.Errorf("failed to get Kubernetes node %s: %w", node.Name, err)
	}

	return GetKubeNodeResult{
		NodeID: input.NodeID,
		Node:   kubeNode,
	}, nil
}
