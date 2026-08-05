// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
		reconciler := newReconciler(c.cfg, c.logger, tx, c.cweTemporalClient)
		return reconciler.reconcile(ctx, cluster)
	})
}
