// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	temporalclient "go.temporal.io/sdk/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// reconciler reconciles control planes.
// It is responsible for ensuring the control plane is healthy and that the control plane nodes are up to date.
type reconciler struct {
	cfg                      config.Config
	logger                   *slog.Logger
	db                       db.Tx
	controllerTemporalClient temporalclient.Client
	k8sClient                ctrlclient.Client
	k8sReader                ctrlclient.Reader

	// providerClient resolves the composed ProviderClient for a cluster.
	// Defaults to clients.GetProviderClient; tests override it with a stub so
	// a function's own error handling can be exercised without going through
	// real NICo/CAPI resolution. reconcile resolves it once per call and
	// passes the specific capability each downstream function needs, rather
	// than each one resolving its own.
	providerClient func(db.Cluster) (clients.ProviderClient, error)

	// requeue is the soonest check-back a step asked for while waiting on
	// something that does not write to the DB (a Temporal workflow). Zero means
	// none; the DB sources and resync drive the next reconcile.
	requeue time.Duration
}

// requeueAfter asks for the cluster to be reconciled again within d. The
// soonest request in one reconcile wins.
func (r *reconciler) requeueAfter(d time.Duration) {
	if d > 0 && (r.requeue == 0 || d < r.requeue) {
		r.requeue = d
	}
}

func newReconciler(
	cfg config.Config,
	logger *slog.Logger,
	db db.Tx,
	controllerTemporalClient temporalclient.Client,
	k8sClient ctrlclient.Client,
	k8sReader ctrlclient.Reader,
) *reconciler {
	return &reconciler{
		cfg:                      cfg,
		logger:                   logger,
		db:                       db,
		controllerTemporalClient: controllerTemporalClient,
		k8sClient:                k8sClient,
		k8sReader:                k8sReader,
		providerClient:           clients.GetProviderClient,
	}
}

func (r *reconciler) reconcile(ctx context.Context, cluster db.Cluster) error {
	// The TeardownCluster workflow deletes CP nodes for a deleting cluster;
	// reconciling scale-up here would keep recreating them and deadlock teardown.
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return nil
	}

	// Resolved once here, as early as cluster is known, and passed down to
	// whatever needs a capability from it -- not a fresh resolve at each spot
	// that needs one.
	provider, err := r.providerClient(cluster)
	if err != nil {
		return fmt.Errorf("resolving provider client: %w", err)
	}

	controlPlane, err := readControlPlane(ctx, r.db, cluster)
	if err != nil {
		return fmt.Errorf("reading control plane: %w", err)
	}

	// The order below is deliberate: everything that does not change etcd
	// membership runs first and is never held up by a delete, and the two steps
	// that do change it -- join and teardown -- are ordered grow before shrink.

	// Status only; no membership change.
	if err := r.reconcileControlPlaneRolloutRequiredCondition(ctx, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane rollout-required conditions: %w", err)
	}
	// The caller holds the cluster row lock. Keep drift observations current,
	// but defer creation, remediation, upgrades, joins and accepted deletes.
	if db.ControlPlaneMutationBlocked(cluster) {
		return nil
	}

	failureDomains, err := r.readFailureDomains(ctx, cluster)
	if err != nil {
		return r.reconcileWithoutFailureDomains(ctx, cluster, controlPlane, err)
	}
	controlPlane.failureDomains = failureDomains
	failureDomains.eligible = placeableDomains(failureDomains.eligible, cluster.Conditions.UnplaceableFailureDomains, time.Now())
	allocator := newControlPlaneDomainAllocator(controlPlane.nodes, failureDomains)

	// Host creation only. A pending or in-flight delete must not stall it: the
	// replacement host is exactly what lets the control plane shrink safely.
	if err := r.reconcileControlPlaneScaleUp(ctx, cluster, controlPlane, allocator, provider.NewNodeProvider()); err != nil {
		return fmt.Errorf("reconciling control plane scale-up: %w", err)
	}

	// Records delete intent only; the teardown is started at the bottom of a later
	// pass. When remediation records a delete, end this pass: controlPlane still
	// holds the pre-remediation snapshot, so the rollout and scale-down steps below
	// would not see the new intent and could queue a second delete without
	// accounting for it. The node write re-enqueues the cluster, and the next
	// reconcile reads a fresh snapshot where the delete is in flight -- rollout and
	// scale-down then defer to it and reconcileRequestedDeletes promotes it.
	replaced, err := r.reconcileStuckPendingNodes(ctx, cluster, controlPlane)
	if err != nil {
		return fmt.Errorf("reconciling stuck pending control plane nodes: %w", err)
	}
	if replaced {
		return nil
	}

	remediated, err := r.remediateMachines(ctx, cluster, controlPlane)
	if err != nil {
		return err
	}
	if remediated {
		return nil
	}

	// Agent replacement can interrupt join and kubeadm RPCs. Deletion and repair
	// still proceed, because they do not require the upgrade to finish.
	if !agentUpgradeBlocksNodeWork(cluster, controlPlane.nodes) {
		// ConfigMap changes only.
		if err := r.reconcileK8sUpgrade(ctx, cluster, controlPlane); err != nil {
			return fmt.Errorf("reconciling k8s upgrade: %w", err)
		}

		// Rollout records delete intent and reconcileRequestedDeletes (below) starts
		// the teardown, so a rollout teardown serializes in the same loop as user
		// deletes, remediation and scale-down.
		rolloutRecorded, err := r.reconcileNodesNeedingRollout(ctx, cluster, controlPlane, allocator, provider.NewNodeProvider())
		if err != nil {
			return fmt.Errorf("reconciling control plane nodes needing rollout: %w", err)
		}
		if rolloutRecorded {
			// Same reason as the remediation return above: controlPlane still holds
			// the pre-rollout snapshot, so the rebalance step below (whose abandon
			// path starts a teardown directly) and scale-down could act without
			// seeing the just-recorded delete. End the pass; the node write
			// re-enqueues the cluster and the next reconcile reads a fresh snapshot
			// where the delete is in flight and every step defers to it.
			return nil
		}

		// Grow before shrink: join a ready host if one is waiting, and leave any
		// queued delete alone until that join is done.
		joinInFlight, err := r.reconcileControlPlaneJoins(ctx, cluster, controlPlane)
		if err != nil {
			return fmt.Errorf("reconciling control plane joins: %w", err)
		}
		if joinInFlight {
			return nil
		}

		// Steady-state rebalance: surge a more-preferred replacement (gated and one
		// at a time), grow-before-shrink, then shed the old node itself once the
		// surge joins. Lowest priority: it yields to the upgrade and rollout steps
		// above by abandoning an in-flight surge.
		rebalanceActed, err := r.reconcileControlPlaneRebalance(ctx, cluster, controlPlane, allocator, provider.NewNodeProvider())
		if err != nil {
			return fmt.Errorf("reconciling control plane rebalance: %w", err)
		}
		// A rebalance that grew, shed or abandoned owns this pass: it already made
		// its one grow-before-shrink change, so return before scale-down (or any
		// later membership step) can act on the same slots off the pre-rebalance
		// snapshot. A recorded shed is promoted to a teardown on the next reconcile,
		// which the fresh snapshot and one-at-a-time promotion drain normally.
		if rebalanceActed {
			return nil
		}
	}

	// Last, and records intent only: scale-up, remediation and rollout grow
	// first, so the control plane never shrinks while it owes a replacement.
	if err := r.reconcileControlPlaneScaleDown(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling control plane scale-down: %w", err)
	}

	// Promote the oldest accepted delete into a teardown, one at a time.
	if _, err := r.reconcileRequestedDeletes(ctx, cluster, controlPlane); err != nil {
		return fmt.Errorf("reconciling requested control plane deletes: %w", err)
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
// failureDomainRetryInterval is how soon a reconcile that could not read failure
// domains runs again.
const (
	failureDomainRetryInterval = 30 * time.Second
)

// reconcileWithoutFailureDomains runs the steps that place no node -- remediation
// and promotion of recorded deletes -- when failure-domain discovery fails, so a
// dead node can still be removed. It returns nil and requeues rather than erroring
// because an error rolls back the reconcile's transaction, undoing those writes.
func (r *reconciler) reconcileWithoutFailureDomains(ctx context.Context, cluster db.Cluster, cp *controlPlane, discoveryErr error) error {
	r.logger.Warn("Control-plane failure-domain discovery failed; running only remediation and recorded deletes",
		"clusterID", cluster.ID, "clusterName", cluster.Name, "err", discoveryErr)
	r.requeueAfter(failureDomainRetryInterval)

	remediated, err := r.remediateMachines(ctx, cluster, cp)
	if err != nil {
		return err
	}
	if remediated {
		return nil
	}
	if _, err := r.reconcileRequestedDeletes(ctx, cluster, cp); err != nil {
		return fmt.Errorf("reconciling requested control plane deletes: %w", err)
	}
	return nil
}

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
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
) error {
	deficit := cluster.ControlPlaneConfig.DesiredCount - controlPlane.size()
	if deficit <= 0 {
		return nil
	}

	// Create exactly the deficit as type-less pending nodes. Instance-type
	// selection and Forge capacity checks are the control plane node scheduler's job: it picks
	// each node's Forge instance type from the pinned InstanceTypePreference at
	// schedule time and persists it back onto the node row.
	created, err := r.scaleUp(ctx, cluster, deficit, "control-plane scale-up", allocator, newNodeProvider)
	if err != nil {
		return fmt.Errorf("scaling up control-plane nodes: %w", err)
	}
	if created == 0 {
		return nil
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
