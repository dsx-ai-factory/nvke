// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controlplane provides controllers for managing Kubernetes control planes.
package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// Controller manages control plane reconciliation for all clusters within a site.
// It handles both node remediation (recycling unhealthy nodes) and OS profile upgrades
// (rolling upgrades when the OS profile changes).
type Controller struct {
	cfg               config.Config
	db                db.DB
	logger            *slog.Logger
	cweTemporalClient client.Client
}

// NewController creates a new control plane Controller.
func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	cweTemporalClient client.Client,
) *Controller {
	return &Controller{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger.With("controller", "control-plane"),
		cweTemporalClient: cweTemporalClient,
	}
}

// Reconcile iterates over all clusters in the site, reconciling each cluster's
// control plane state within its own transaction.
func (c *Controller) Reconcile(ctx context.Context, site db.Site) error {
	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("listing clusters: %w", err)
	}

	for _, cluster := range clusters {
		if err := c.reconcileClusterInTransaction(ctx, cluster); err != nil {
			c.logger.Error("failed to reconcile cluster, continuing with other clusters",
				"cluster_id", cluster.ID,
				"cluster_name", cluster.Name,
				"error", err)
		}
	}
	return nil
}

func (c *Controller) reconcileClusterInTransaction(ctx context.Context, cluster db.Cluster) error {
	return c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Re-read under a row lock: the cluster was listed outside any transaction,
		// so the reconciler could otherwise act on a stale snapshot and race the
		// teardown workflow (e.g. re-provision CP nodes for a cluster being deleted).
		// GetClusterForUpdate refreshes the snapshot and serializes against teardown
		// for the duration of this transaction.
		lockedCluster, err := tx.GetClusterForUpdate(ctx, cluster.ID)
		if err != nil {
			return fmt.Errorf("locking cluster %s for reconcile: %w", cluster.ID, err)
		}
		if lockedCluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}
		reconciler := newReconciler(c.cfg, c.logger, tx, c.cweTemporalClient)
		return reconciler.reconcile(ctx, lockedCluster)
	})
}
