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
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ApplyNodeConditionInput struct {
	NodeID        uuid.UUID
	NodeCondition corev1.NodeCondition
}

type ApplyNodeConditionResult struct {
	Output string
}

func (a *Activities) ApplyNodeCondition(ctx context.Context, input ApplyNodeConditionInput) (ApplyNodeConditionResult, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ApplyNodeConditionResult{}, nonRetryableIfNotFound("failed to get node", err)
	}

	if input.NodeCondition.Type == "" {
		return ApplyNodeConditionResult{}, fmt.Errorf("node condition type is required")
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return ApplyNodeConditionResult{}, nonRetryableIfNotFound("failed to get cluster", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return ApplyNodeConditionResult{}, err
	}

	conditionPatch := struct {
		Status struct {
			Conditions []corev1.NodeCondition `json:"conditions"`
		} `json:"status"`
	}{}
	conditionPatch.Status.Conditions = []corev1.NodeCondition{input.NodeCondition}
	patch, err := json.Marshal(conditionPatch)
	if err != nil {
		return ApplyNodeConditionResult{}, fmt.Errorf("failed to marshal node condition patch: %w", err)
	}

	logger.Info("Applying node condition",
		"nodeID", input.NodeID,
		"nodeName", node.Name,
		"conditionType", input.NodeCondition.Type,
	)

	kubeNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}
	if err := tenantClient.Status().Patch(ctx, kubeNode, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
		return ApplyNodeConditionResult{}, fmt.Errorf("failed to apply condition %s to Kubernetes node %s: %w", input.NodeCondition.Type, node.Name, err)
	}

	return ApplyNodeConditionResult{Output: fmt.Sprintf("node/%s condition %s applied", node.Name, input.NodeCondition.Type)}, nil
}
