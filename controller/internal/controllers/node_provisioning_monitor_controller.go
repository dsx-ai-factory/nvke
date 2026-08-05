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
	"fmt"
	"log/slog"
	"time"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

// NodeProvisioningMonitorController watches nodes that are in provisioning
// and recycles them once they exceed the configured timeout.
type NodeProvisioningMonitorController struct {
	cfg    config.Config
	db     db.DB
	logger *slog.Logger
}

func NewNodeProvisioningMonitorController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
) *NodeProvisioningMonitorController {
	return &NodeProvisioningMonitorController{
		cfg:    cfg,
		db:     dbInstance,
		logger: logger,
	}
}

func (c *NodeProvisioningMonitorController) Reconcile(ctx context.Context, site db.Site) error {
	if c.cfg.NodeProvisioningTimeout <= 0 {
		c.logger.Debug("Node provisioning monitor disabled, skipping reconciliation")
		return nil
	}

	return c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		dbNodes, err := tx.ListNodes(ctx, db.NodeFilters{
			SiteID:       &site.ID,
			Provisioning: pointerTo(true),
			Deleting:     pointerTo(false),
		})
		if err != nil {
			return fmt.Errorf("failed to list nodes: %w", err)
		}

		now := time.Now()
		for _, dbNode := range dbNodes {
			provisioningAge, shouldRecycle := shouldRecycleProvisioningNode(now, dbNode, c.cfg.NodeProvisioningTimeout)
			if !shouldRecycle {
				continue
			}

			_, nodePool, err := nodes.GetNodeContext(ctx, tx, dbNode)
			if err != nil {
				return fmt.Errorf("failed to resolve node context for node %s: %w", dbNode.ID, err)
			}

			logger := c.logger.With(
				"clusterID", nodePool.ClusterID,
				"nodePoolID", nodePool.ID,
				"nodePoolName", nodePool.Name,
				"nodeID", dbNode.ID,
				"nodeName", dbNode.Name,
				"forgeInstanceID", dbNode.ForgeInstanceID,
				"forgeMachineID", dbNode.ForgeMachineID,
				"nodeProvisioningTimeout", c.cfg.NodeProvisioningTimeout.String(),
				"provisioningAge", provisioningAge.String(),
			)

			if nodePool.ControlPlane {
				logger.Info("Provisioning monitor does not recycle control plane nodes, skipping stuck-node recycling")
				continue
			}

			if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
				logger.Debug("Node pool is being deleted, skipping node recycling")
				continue
			}

			if nodePool.ProvisioningMonitorDisabled {
				logger.Info("Provisioning monitor paused for node pool, skipping stuck-node recycling")
				continue
			}

			cluster, err := tx.GetCluster(ctx, nodePool.ClusterID)
			if err != nil {
				return fmt.Errorf("failed to get cluster for node %s: %w", dbNode.ID, err)
			}

			logger.Warn("Recycling worker node stuck in provisioning")
			reason := fmt.Sprintf("Node provisioning exceeded timeout of %s", c.cfg.NodeProvisioningTimeout)
			err = nodes.DeleteWithTx(ctx, tx, dbNode.ID, nodes.WithReason(reason))
			if err != nil {
				return fmt.Errorf("failed to recycle node %s stuck provisioning: %w", dbNode.ID, err)
			}

			metrics.RecordNodeProvisioningGiveup(
				site.ID.String(),
				nodePool.ClusterID.String(),
				nodePool.ID.String(),
				cluster.Name,
				nodePool.Name,
			)
		}

		return nil
	})
}

func shouldRecycleProvisioningNode(now time.Time, node db.Node, timeout time.Duration) (time.Duration, bool) {
	if node.Conditions.Provisioning.Status != db.ConditionStatusTrue {
		return 0, false
	}
	if node.Conditions.Provisioned.Status == db.ConditionStatusTrue {
		return 0, false
	}
	if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return 0, false
	}

	provisioningStartedAt := node.Conditions.Provisioning.LastTransitionTime
	if provisioningStartedAt.IsZero() {
		provisioningStartedAt = node.CreatedAt
	}

	provisioningAge := now.Sub(provisioningStartedAt)
	shouldRecycle := provisioningAge > timeout

	return provisioningAge, shouldRecycle
}
