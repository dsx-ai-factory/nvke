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
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DeleteKubeNodeInput struct {
	NodeID uuid.UUID
}

func (a *Activities) DeleteKubeNode(ctx context.Context, input DeleteKubeNodeInput) error {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return err
	}

	logger.Info("Deleting Kubernetes node", "nodeID", input.NodeID, "nodeName", node.Name)
	kubeNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}
	if err := tenantClient.Delete(ctx, kubeNode); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Kubernetes node %s: %w", node.Name, err)
	}

	return nil
}
