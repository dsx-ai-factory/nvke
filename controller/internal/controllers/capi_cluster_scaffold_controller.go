// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type CAPIClusterScaffoldController struct {
	logger          *slog.Logger
	db              db.DB
	k8sClient       ctrlclient.Client
	nodeAgentClient agent.Client
}

func NewCAPIClusterScaffoldController(
	logger *slog.Logger,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
) *CAPIClusterScaffoldController {
	return &CAPIClusterScaffoldController{
		logger:          logger,
		db:              dbInstance,
		k8sClient:       k8sClient,
		nodeAgentClient: nodeAgentClient,
	}
}

func (c *CAPIClusterScaffoldController) Reconcile(ctx context.Context, site db.Site) error {
	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	var reconcileErrs []error
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			c.logger.Debug("Cluster is deleting, skipping CAPI cluster scaffold reconciliation", "clusterID", cluster.ID, "clusterName", cluster.Name)
			continue
		}
		if capi.IsEmptyManifest(cluster.CapiInfraCluster) {
			continue
		}

		if err := c.reconcileCluster(ctx, cluster); err != nil {
			c.logger.Error("Failed to reconcile CAPI cluster scaffold", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	return errors.Join(reconcileErrs...)
}

func (c *CAPIClusterScaffoldController) reconcileCluster(ctx context.Context, cluster db.Cluster) error {
	if err := capi.ApplyClusterScaffold(ctx, c.k8sClient, cluster); err != nil {
		return fmt.Errorf("failed to apply CAPI cluster scaffold: %w", err)
	}

	if err := c.reconcileKubeconfigSecret(ctx, cluster); err != nil {
		return fmt.Errorf("failed to reconcile CAPI kubeconfig secret: %w", err)
	}

	return nil
}

func (c *CAPIClusterScaffoldController) reconcileKubeconfigSecret(ctx context.Context, cluster db.Cluster) error {
	controlPlaneNodes, err := c.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &cluster.ID,
		ControlPlane: ptr.To(true),
	})
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	candidates, healthyCandidates := nodes.PickControlPlaneRunbookCandidates(controlPlaneNodes)
	if len(candidates) == 0 {
		return fmt.Errorf("cluster %s has no joined control plane nodes for kubeconfig extraction", cluster.ID)
	}
	if !healthyCandidates {
		c.logger.Warn("No healthy joined control plane nodes found, falling back to joined nodes for kubeconfig extraction", "clusterID", cluster.ID, "clusterName", cluster.Name)
	}

	var candidateErrs []error
	for _, node := range candidates {
		kubeconfigBytes, err := c.nodeAgentClient.GetKubeconfig(ctx, node)
		if err != nil {
			candidateErrs = append(candidateErrs, fmt.Errorf("node %s: %w", node.ID, err))
			continue
		}
		if kubeconfigBytes == "" {
			candidateErrs = append(candidateErrs, fmt.Errorf("node %s returned an empty kubeconfig", node.ID))
			continue
		}

		return capi.ApplyClusterKubeconfigSecret(ctx, c.k8sClient, cluster, []byte(kubeconfigBytes))
	}

	return fmt.Errorf("failed to extract kubeconfig from %d control plane candidates: %w", len(candidates), errors.Join(candidateErrs...))
}
