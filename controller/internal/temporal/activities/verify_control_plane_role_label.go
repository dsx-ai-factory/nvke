// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// VerifyControlPlaneRoleLabel returns an error when the node's Kubernetes Node lacks the control-plane role label.
// kubeadm writes the label when the node joins as a control plane, and callers cannot set it through the API,
// so a missing label means the join did not mark the node. The node agent selects control-plane nodes by this label.
func (a *Activities) VerifyControlPlaneRoleLabel(ctx context.Context, nodeID uuid.UUID) error {
	node, err := a.db.GetNode(ctx, nodeID)
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

	var kubeNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: node.Name}, &kubeNode); err != nil {
		return fmt.Errorf("failed to get Kubernetes node %s: %w", node.Name, err)
	}

	if _, found := kubeNode.Labels[controllerconfig.NodeMetadataControlPlaneRoleKey]; !found {
		return fmt.Errorf("control-plane node/%s is missing expected label %s", node.Name, controllerconfig.NodeMetadataControlPlaneRoleKey)
	}

	return nil
}
