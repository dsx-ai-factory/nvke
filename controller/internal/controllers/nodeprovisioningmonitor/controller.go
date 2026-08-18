// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeprovisioningmonitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

// Controller watches nodes that are in provisioning
// and recycles them once they exceed the configured timeout.
type Controller struct {
	cfg    config.Config
	db     *dbwrapper.DB
	logger *slog.Logger
}

func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
) *Controller {
	return &Controller{
		cfg:    cfg,
		db:     dbInstance,
		logger: logger,
	}
}

func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-provisioning-monitor",
		c,
		c.db.NodeSource(db.NodeFilters{
			Provisioning: ptr.To(true),
			Deleting:     ptr.To(false),
		}),
	)
}

func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	if c.cfg.NodeProvisioningTimeout <= 0 {
		c.logger.Debug("Node provisioning monitor disabled, skipping reconciliation", "nodeID", key.ID)
		return reconcile.Result{}, nil
	}

	result := reconcile.Result{}
	err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		node, err := tx.GetNodeForUpdate(ctx, key.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				c.logger.Debug("Node not found, skipping provisioning monitor reconciliation", "nodeID", key.ID)
				return nil
			}
			return fmt.Errorf("failed to get node %s: %w", key.ID, err)
		}

		result, err = c.reconcile(ctx, tx, node)
		return err
	})
	return result, err
}

func (c *Controller) reconcile(ctx context.Context, tx db.Tx, dbNode db.Node) (reconcile.Result, error) {
	provisioningAge, shouldRecycle := shouldRecycleProvisioningNode(time.Now(), dbNode, c.cfg.NodeProvisioningTimeout)
	if !shouldRecycle {
		if provisioningAge > 0 {
			return reconcile.Result{RequeueAfter: c.cfg.NodeProvisioningTimeout - provisioningAge}, nil
		}
		return reconcile.Result{}, nil
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, tx, dbNode)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to resolve node context for node %s: %w", dbNode.ID, err)
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
		return reconcile.Result{}, nil
	}

	if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
		logger.Debug("Node pool is being deleted, skipping node recycling")
		return reconcile.Result{}, nil
	}

	if nodePool.ProvisioningMonitorDisabled {
		logger.Info("Provisioning monitor paused for node pool, skipping stuck-node recycling")
		return reconcile.Result{}, nil
	}

	cluster, err := tx.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get cluster for node %s: %w", dbNode.ID, err)
	}

	logger.Warn("Recycling worker node stuck in provisioning")
	reason := fmt.Sprintf("Node provisioning exceeded timeout of %s", c.cfg.NodeProvisioningTimeout)
	err = nodes.DeleteWithTx(ctx, tx, dbNode.ID,
		nodes.WithReason(reason),
		// Controller already filters pool/CP; skip live-API guard so recycle
		// does not take the cluster row lock.
		nodes.WithSkipLiveDeleteGuard(),
	)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to recycle node %s stuck provisioning: %w", dbNode.ID, err)
	}

	metrics.RecordNodeProvisioningGiveup(
		cluster.SiteID.String(),
		nodePool.ClusterID.String(),
		nodePool.ID.String(),
		cluster.Name,
		nodePool.Name,
	)

	return reconcile.Result{}, nil
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
