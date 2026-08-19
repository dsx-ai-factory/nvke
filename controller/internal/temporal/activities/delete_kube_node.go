// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/nke/controller/internal/db"
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

	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		logger.Info("Cluster is deleting, skipping Kubernetes node deletion", "nodeID", input.NodeID, "clusterID", cluster.ID)
		return nil
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
