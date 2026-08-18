// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// reconciler reconciles control planes.
// It is responsible for ensuring the control plane is healthy and that the control plane nodes are up to date.
type reconciler struct {
	cfg               config.Config
	logger            *slog.Logger
	db                db.Tx
	cweTemporalClient client.Client
}

func newReconciler(
	cfg config.Config,
	logger *slog.Logger,
	db db.Tx,
	cweTemporalClient client.Client,
) *reconciler {
	return &reconciler{
		cfg:               cfg,
		logger:            logger,
		db:                db,
		cweTemporalClient: cweTemporalClient,
	}
}

func (r *reconciler) reconcile(ctx context.Context, cluster db.Cluster) error {
	// The TeardownCluster workflow deletes CP nodes for a deleting cluster;
	// reconciling scale-up here would keep recreating them and deadlock teardown.
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return nil
	}

	controlPlane, err := readControlPlane(ctx, r.db, cluster.ID)
	if err != nil {
		return fmt.Errorf("reading control plane: %w", err)
	}

	if err := r.reconcileControlPlaneScaleUp(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane scale-up: %w", err)
	}

	// Reconcile node remediation (replace marked nodes)
	if requeue, err := r.remediateMachines(ctx, cluster, controlPlane); err != nil {
		return err
	} else if requeue {
		return nil
	}

	// Reconcile Kubernetes version upgrades (if cluster K8s version changed)
	if err := r.reconcileK8sUpgrade(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling k8s upgrade: %w", err)
	}

	// Tear down deprecated control plane nodes (e.g. OS profile rolling upgrades).
	if err := r.reconcileDeprecatedControlPlaneNodes(ctx, cluster); err != nil {
		return fmt.Errorf("reconciling deprecated control plane nodes: %w", err)
	}

	return nil
}

// reconcileControlPlaneScaleUp provisions control-plane nodes to
// match cluster.ControlPlaneConfig.DesiredCount. It is the single writer that
// drives the CP node count.
func (r *reconciler) reconcileControlPlaneScaleUp(
	ctx context.Context,
	cluster db.Cluster,
	controlPlane *controlPlane,
) error {
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return nil
	}

	deficit := cluster.ControlPlaneConfig.DesiredCount - controlPlane.size()
	if deficit <= 0 {
		return nil
	}

	// Create exactly the deficit as type-less pending nodes. Instance-type
	// selection and Forge capacity checks are the node scheduler's job: it picks
	// each node's Forge instance type from the pinned InstanceTypePreference at
	// schedule time and persists it back onto the node row.
	created, err := r.scaleUp(ctx, cluster, deficit, "control-plane scale-up")
	if err != nil {
		return fmt.Errorf("scaling up control-plane nodes: %w", err)
	}

	r.logger.Info(
		"control-plane scale-up reconciled",
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"desiredCount", cluster.ControlPlaneConfig.DesiredCount,
		"existingCount", controlPlane.size(),
		"deficit", deficit,
		"createdCount", created,
	)

	return nil
}
