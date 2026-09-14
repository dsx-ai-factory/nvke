// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
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

	controlPlane, err := readControlPlane(ctx, r.db, cluster, r.cfg.UseControlPlaneControllerForControlPlane())
	if err != nil {
		return fmt.Errorf("reading control plane: %w", err)
	}

	// Refresh each managed node's rollout-required condition before acting, so the
	// reported state reflects current drift regardless of which branch runs below.
	if err := r.reconcileControlPlaneRolloutRequiredCondition(ctx, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane rollout-required conditions: %w", err)
	}

	if err := r.reconcileControlPlaneScaleUp(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane scale-up: %w", err)
	}

	// Join ready control-plane hosts one at a time (serial join). Host
	// provisioning may run in parallel; only the kubeadm join is serialized.
	if err := r.reconcileControlPlaneJoins(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane joins: %w", err)
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

	// Tear down control plane nodes needing rollout (e.g. OS profile rolling upgrades).
	if err := r.reconcileNodesNeedingRollout(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane nodes needing rollout: %w", err)
	}

	return nil
}

// reconcileControlPlaneRolloutRequiredCondition refreshes each
// control-plane-controller node's ControlPlaneRolloutRequired condition to reflect
// current drift from the cluster's desired configuration. It maintains the
// condition on every managed node (True when drifted, False when converged)
// independent of the recycling gate, so the "drifted but recycling disabled" case
// is still surfaced.
//
// The drift is computed against the in-memory node first, so the steady state
// (no change) takes no row locks and issues no writes. Only when the condition
// would actually flip is the node locked and recomputed before the write, so a
// concurrent condition writer is not clobbered (same pattern as the
// node-agent-readiness controller).
func (r *reconciler) reconcileControlPlaneRolloutRequiredCondition(ctx context.Context, cp *controlPlane) error {
	for i := range cp.nodes {
		if !cp.nodes[i].IsManagedByControlPlaneController() {
			continue
		}
		// Cheap check against the in-memory copy; skip untouched nodes lock-free.
		probe := cp.nodes[i]
		if !cp.refreshControlPlaneRolloutRequiredCondition(&probe) {
			continue
		}
		lockedNode, err := r.db.GetNodeForUpdate(ctx, cp.nodes[i].ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			return fmt.Errorf("locking node %s: %w", cp.nodes[i].ID, err)
		}
		if !cp.refreshControlPlaneRolloutRequiredCondition(&lockedNode) {
			continue
		}
		if _, err := r.db.UpdateNode(ctx, &lockedNode); err != nil {
			return fmt.Errorf("persisting control plane rollout-required condition for node %s: %w", lockedNode.ID, err)
		}
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
	// selection and Forge capacity checks are the control plane node scheduler's job: it picks
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
