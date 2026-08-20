// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controlplane provides controllers for managing Kubernetes control planes.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// Controller manages keyed control plane reconciliation for clusters.
// It handles scale-up, remediation, Kubernetes upgrades, and deprecated-node cleanup.
type Controller struct {
	cfg               config.Config
	db                *dbwrapper.DB
	logger            *slog.Logger
	cweTemporalClient client.Client
}

// NewController creates a new control plane Controller.
func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
	cweTemporalClient client.Client,
) *Controller {
	return &Controller{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger.With("controller", "control-plane"),
		cweTemporalClient: cweTemporalClient,
	}
}

// SetupWithManager wires the controller into controller-runtime with DB-backed sources.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"control-plane-controller",
		c,
		c.db.ClusterSource(dbfilters.Cluster{}),
		c.db.NodePoolSource(dbfilters.NodePool{ControlPlane: ptr.To(true)}, nodePoolClusterKeys),
		c.db.NodeSource(dbfilters.Node{ControlPlane: ptr.To(true)}, nodeClusterKeys),
	)
}

func nodePoolClusterKeys(nodePool db.NodePool) []dbreconcile.Key {
	if nodePool.ClusterID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(nodePool.ClusterID)}
}

func nodeClusterKeys(node db.Node) []dbreconcile.Key {
	if node.ClusterID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(node.ClusterID)}
}

// Reconcile reconciles one cluster's control plane from a ClusterID root key.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	result := reconcile.Result{}
	err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Re-read under a row lock: the cluster may have been enqueued from a stale
		// event, so the reconciler could otherwise act on a stale snapshot and race
		// teardown (e.g. re-provision CP nodes for a cluster being deleted).
		lockedCluster, err := tx.GetClusterForUpdate(ctx, key.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				c.logger.Debug("cluster not found, skipping control-plane reconciliation", "clusterID", key.ID.String())
				return nil
			}
			return fmt.Errorf("locking cluster %s for reconcile: %w", key.ID, err)
		}

		if lockedCluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}

		reconciler := newReconciler(c.cfg, c.logger, tx, c.cweTemporalClient)
		return reconciler.reconcile(ctx, lockedCluster)
	})
	return result, err
}
