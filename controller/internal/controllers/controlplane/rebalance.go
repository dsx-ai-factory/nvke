// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

const (
	// rebalanceSnapshotReason tags the transient, preference-capped config
	// snapshots surge nodes pin to, distinguishing them from operator changes.
	rebalanceSnapshotReason = "internal: rebalance preference cap"

	// defaultRebalanceSurgeTimeout bounds the capacity-wait phase of a surge: how
	// long a not-yet-allocated surge may wait for the provider to place an instance
	// of a more-preferred type before the step gives up and backs off. It governs
	// only the not-yet-allocated phase and is deliberately kept separate from the
	// node provisioning timeout: once an instance is allocated the surge is an
	// ordinary provisioning node, bounded by NODE_PROVISIONING_TIMEOUT (which does
	// not yet recycle control-plane nodes), not by this timeout.
	defaultRebalanceSurgeTimeout = 45 * time.Minute

	// rebalanceShedReason is the delete reason recorded when rebalance sheds the
	// old, less-preferred node its surge replaced.
	rebalanceShedReason = "Control-plane rebalance: shedding a less-preferred node after its replacement joined"

	// rebalanceUndoSurgeReason is recorded when a joined surge is not strictly
	// better than any other member. Rebalance removes that surge itself so the
	// extra node does not stay when automatic scale-down is off.
	rebalanceUndoSurgeReason = "Control-plane rebalance: removing a surge that joined no better than the members it was meant to replace"

	// rebalanceBackoffInterval rate-limits re-probing after a surge is abandoned
	// for lack of preferred capacity: the step records the abandonment and skips
	// starting another surge until this elapses, so a cluster with no preferred
	// capacity re-probes slowly instead of churning a surge every timeout. A surge
	// that joins and sheds the old node is progress, not a failure, and records no
	// backoff -- the next round follows immediately, so the control plane converges
	// onto the preferred types as fast as capacity allows.
	rebalanceBackoffInterval = 3 * time.Hour
)

// reconcileControlPlaneRebalance moves the control plane onto more-preferred
// instance types at steady state: it surges one more-preferred replacement for a
// joined member on a less-preferred type, then sheds the old node through the
// shared delete path (quorum floor applied) once the replacement joins. It is the
// lowest-priority step -- it acts only when nothing else is in flight, yields to
// upgrades and rollouts by abandoning an in-flight surge, and backs off re-probing
// to once per rebalanceBackoffInterval only after a surge is abandoned for lack of
// capacity -- a successful surge-and-shed round is not throttled.
//
// It reports whether it changed control-plane membership this pass: a pass that
// surges, sheds or abandons returns true so the caller returns before scale-down,
// keeping any later membership step off the pre-rebalance snapshot.
//
// It is provider-neutral: it surges via the ControlPlaneScaleDownRanker seam and
// bounds the fallback via the ControlPlaneRebalanceCapper seam. A provider with no
// ordered preference implements neither, so the step is a no-op for it.
func (r *reconciler) reconcileControlPlaneRebalance(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
) (bool, error) {
	ranker := cluster.ControlPlaneConfig.ScaleDownRanker()
	if ranker == nil {
		return false, nil
	}

	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
	)

	// An in-flight surge takes precedence: resolve it (shed + clear on join) or
	// abandon it (yield / timeout) before considering a new one. Resolve it even
	// when rebalancing has since been disabled, so flipping the opt-out mid-surge
	// does not strand the extra node -- an unjoined surge is never abandoned and a
	// joined one is never shed, leaving the control plane above its desired count.
	if surge, ok := cp.rebalanceSurge(); ok {
		return r.reconcileRebalanceSurge(ctx, cluster, cp, surge, logger)
	}

	// Gate only new surges on the opt-out; an existing surge is resolved above.
	if cluster.ControlPlaneConfig.NodeRebalanceDisabled {
		return false, nil
	}

	// Only surge at steady state, never while a higher-priority operation owns the
	// same slots. The rollout check also covers drift with recycling disabled.
	if cp.upgradeInProgress(cluster) {
		return false, nil
	}
	if cp.countNodesNeedingRollout() > 0 {
		return false, nil
	}
	// A control plane below the fault-tolerant floor can never shed the node a
	// surge adds: the quorum floor refuses the shed, so it would sit one node
	// above desired forever. Leave a sub-floor control plane alone.
	if cluster.ControlPlaneConfig.DesiredCount < nodes.MinFaultTolerantControlPlaneNodes {
		return false, nil
	}
	if cp.size() != cluster.ControlPlaneConfig.DesiredCount {
		return false, nil
	}
	if _, inFlight := cp.nodeWithDeleteInFlight(); inFlight {
		return false, nil
	}

	eligible := cp.scaleDownEligible()
	if !controlPlaneHasNodeBelowPrimaryRank(eligible, ranker) {
		return false, nil
	}
	// Back off only after a surge was abandoned for lack of capacity: re-probe
	// slowly rather than churn a surge every timeout. Successful rounds record no
	// backoff and flow straight through.
	if rebalanceCoolingDown(cluster) {
		logger.Debug("Control-plane rebalance backing off after an abandoned surge; skipping",
			"lastBackoff", cluster.Conditions.ControlPlaneRebalanceBackoff.LastTransitionTime,
			"interval", rebalanceBackoffInterval,
		)
		return false, nil
	}
	if r.controlPlaneReplacementBlocked(cluster) {
		logger.Warn("Skipping control-plane rebalance surge because controlPlaneVIP is not configured")
		return false, nil
	}

	// Cap the surge's fallback to types strictly better than the worst eligible
	// node; the cap rides the transient snapshot the surge pins to.
	capRank := worstControlPlaneRank(eligible, ranker)
	cappedSpec, ok := cluster.ControlPlaneConfig.CappedControlPlaneConfig(capRank)
	if !ok {
		// Provider ranks but cannot express a capped preference; do not surge.
		return false, nil
	}
	snapshotID, err := db.FindOrCreateControlPlaneConfiguration(ctx, r.db, cluster.ID, cappedSpec, rebalanceSnapshotReason)
	if err != nil {
		return false, fmt.Errorf("resolving capped control-plane config snapshot: %w", err)
	}

	created, err := r.scaleUp(ctx, cluster, 1, "control-plane rebalance surge", allocator, newNodeProvider, withRebalanceReplacement(snapshotID))
	if err != nil {
		return false, fmt.Errorf("surging control-plane rebalance replacement: %w", err)
	}
	if created == 1 {
		logger.Info("Surged control-plane rebalance replacement; sheds the old node itself once it joins",
			"preferenceCap", capRank,
			"configurationID", snapshotID,
		)
		return true, nil
	}
	return false, nil
}

// rebalanceCoolingDown reports whether a surge was abandoned for lack of capacity
// within rebalanceBackoffInterval. A cluster that never recorded one is never
// backing off, so a steady stream of successful surge-and-shed rounds is never
// throttled.
func rebalanceCoolingDown(cluster db.Cluster) bool {
	last := cluster.Conditions.ControlPlaneRebalanceBackoff.LastTransitionTime
	return !last.IsZero() && time.Since(last) < rebalanceBackoffInterval
}

// recordRebalanceBackoff stamps the time a surge was abandoned for lack of
// capacity, which anchors the re-probe backoff. It re-reads the cluster (the
// caller holds the lock) so it extends rather than clobbers conditions written
// earlier in the same reconcile.
func (r *reconciler) recordRebalanceBackoff(ctx context.Context, clusterID uuid.UUID, logger *slog.Logger) error {
	cluster, err := r.db.GetCluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("loading cluster %s to record rebalance backoff: %w", clusterID, err)
	}
	cluster.Conditions.ControlPlaneRebalanceBackoff = db.NewCondition(true, "Abandoned a more-preferred control-plane surge for lack of capacity")
	if _, err := r.db.UpdateCluster(ctx, &cluster); err != nil {
		return fmt.Errorf("recording rebalance backoff for cluster %s: %w", clusterID, err)
	}
	logger.Debug("Recorded control-plane rebalance backoff; next surge waits for the interval", "interval", rebalanceBackoffInterval)
	return nil
}

// reconcileRebalanceSurge drives an in-flight surge to resolution: it finishes the
// surge once it joins, and otherwise abandons it on a yield signal or timeout.
func (r *reconciler) reconcileRebalanceSurge(ctx context.Context, cluster db.Cluster, cp *controlPlane, surge db.Node, logger *slog.Logger) (bool, error) {
	nodeLogger := logger.With("nodeID", surge.ID, "nodeName", surge.Name)

	// Joined and healthy: shed the old node, then clear the marker. A pass that
	// cannot shed yet leaves the marker and reports no membership change, so
	// scale-down and delete promotion still run.
	if isCleanJoinedControlPlaneMember(surge) {
		return r.finishRebalanceSurge(ctx, cluster, cp, surge, nodeLogger)
	}

	// Not yet joined: yield to any higher-priority operation by abandoning the
	// surge (its pin and marker die with the node).
	if cp.upgradeInProgress(cluster) || cp.countNodesNeedingRollout() > 0 {
		started, err := r.abandonRebalanceSurge(ctx, cp, surge, "yielding to a control-plane upgrade or rollout", nodeLogger)
		if err != nil {
			return false, err
		}
		// If a delete is already in flight the teardown did not start; report no
		// change so the reconcile continues to reconcileRequestedDeletes.
		return started, nil
	}

	// Capacity-wait phase only: a surge that has waited past the surge timeout
	// without the provider allocating an instance could not place any type in the
	// capped preference, so this is a genuine capacity shortfall. Give up, then
	// back off so the next round re-probes slowly instead of churning a surge every
	// timeout. Once an instance is allocated the surge found capacity and is an
	// ordinary provisioning node: its progress is bounded by the node provisioning
	// timeout (NODE_PROVISIONING_TIMEOUT, which does not yet recycle control-plane
	// nodes), not by this step, so keep waiting rather than abandoning it here.
	if rebalanceSurgeExpired(surge) && !surge.HasAllocatedInstance() {
		started, err := r.abandonRebalanceSurge(ctx, cp, surge, "no capacity for a more-preferred instance type", nodeLogger)
		if err != nil {
			return false, err
		}
		if !started {
			// A delete is already in flight for the surge; let the reconcile fall
			// through to reconcileRequestedDeletes rather than short-circuiting and
			// re-stamping the backoff on every pass.
			return false, nil
		}
		if err := r.recordRebalanceBackoff(ctx, cluster.ID, nodeLogger); err != nil {
			return false, err
		}
		return true, nil
	}

	nodeLogger.Debug("Control-plane rebalance surge coming up; waiting")
	return false, nil
}

// finishRebalanceSurge completes a joined surge: it sheds the old node through the
// shared delete path, then clears the marker. It reports whether this pass
// finished the surge.
//
// The marker is cleared only when a node was actually removed for this surge, or
// when the control plane is already back at its desired count and nothing worse
// remains to shed. Clearing it after skipping the shed would leave the cluster
// one node above desiredCount. Rebalance does not depend on automatic scale-down,
// so nothing else would remove that surplus, and the size gate would then refuse
// every later surge.
func (r *reconciler) finishRebalanceSurge(ctx context.Context, cluster db.Cluster, cp *controlPlane, surge db.Node, logger *slog.Logger) (bool, error) {
	ranker := cluster.ControlPlaneConfig.ScaleDownRanker()

	// A delete already in flight clears the marker only when that node is a joined
	// member the surge strictly improves on: the shed from an earlier pass whose
	// marker clear did not land, or another removal that already does the same
	// job. An unrelated delete must not clear the marker.
	if inFlight, ok := cp.nodeWithDeleteInFlight(); ok {
		if inFlightDeleteImprovesOnSurge(inFlight, surge, ranker) {
			if err := r.clearRebalanceMarker(ctx, surge, logger); err != nil {
				return false, err
			}
			return true, nil
		}
		logger.Debug("Rebalance surge joined but another control-plane delete is in flight; keeping the marker until it can shed",
			"inFlightNodeID", inFlight.ID,
			"inFlightNodeName", inFlight.Name,
		)
		return false, nil
	}

	// Shed the least-preferred joined member other than the surge, and only when
	// it ranks strictly worse.
	victim, ok := leastPreferredControlPlaneNodeToShed(cp, surge, ranker)
	if !ok {
		return r.finishRebalanceSurgeWithoutVictim(ctx, cluster, cp, surge, logger)
	}

	recorded, err := r.recordControlPlaneManagedDelete(ctx, logger, victim, rebalanceShedReason)
	if err != nil {
		return false, err
	}
	if !recorded {
		return r.finishRebalanceSurgeFloorRefused(ctx, cluster, cp, surge, victim, logger)
	}
	logger.Info("Rebalance surge joined; recorded delete of the less-preferred node it replaced",
		"victimID", victim.ID,
		"victimName", victim.Name,
	)
	if err := r.clearRebalanceMarker(ctx, surge, logger); err != nil {
		return false, err
	}
	return true, nil
}

// finishRebalanceSurgeWithoutVictim runs when the joined surge is not strictly
// better than any other member. Above desiredCount it removes the surge, which
// is the extra node. At desiredCount the surge already filled a departure, so
// it becomes an ordinary member.
func (r *reconciler) finishRebalanceSurgeWithoutVictim(ctx context.Context, cluster db.Cluster, cp *controlPlane, surge db.Node, logger *slog.Logger) (bool, error) {
	if cp.size() <= cluster.ControlPlaneConfig.DesiredCount {
		logger.Info("Rebalance surge joined with nothing worse to shed and the control plane is at its desired count; clearing the marker")
		if err := r.clearRebalanceMarker(ctx, surge, logger); err != nil {
			return false, err
		}
		return true, nil
	}

	recorded, err := r.recordControlPlaneManagedDelete(ctx, logger, surge, rebalanceUndoSurgeReason)
	if err != nil {
		return false, err
	}
	if !recorded {
		logger.Debug("Quorum floor refused removal of a rebalance surge that is not an improvement; keeping the marker")
		return false, nil
	}
	logger.Info("Rebalance surge joined no better than the members it was meant to replace; recorded its removal")
	return true, nil
}

// finishRebalanceSurgeFloorRefused runs when the shed was refused by the quorum
// floor. Above desiredCount the marker stays so the next pass retries the shed.
// At desiredCount the shed cannot succeed until another node joins, so the marker
// is cleared and a later pass surges a replacement the usual way.
func (r *reconciler) finishRebalanceSurgeFloorRefused(ctx context.Context, cluster db.Cluster, cp *controlPlane, surge, victim db.Node, logger *slog.Logger) (bool, error) {
	if cp.size() > cluster.ControlPlaneConfig.DesiredCount {
		logger.Debug("Rebalance surge joined but the quorum floor refused the shed; keeping the marker",
			"victimID", victim.ID,
			"victimName", victim.Name,
		)
		return false, nil
	}
	logger.Info("Rebalance shed refused by the quorum floor at the desired count; clearing the marker so a later surge can replace the old node",
		"victimID", victim.ID,
		"victimName", victim.Name,
	)
	if err := r.clearRebalanceMarker(ctx, surge, logger); err != nil {
		return false, err
	}
	return true, nil
}

// inFlightDeleteImprovesOnSurge reports whether a delete already in flight is of
// a joined member the surge strictly improves on. A delete of the surge itself,
// of a node that is not yet joined, or of a node the surge does not improve on
// is unrelated.
func inFlightDeleteImprovesOnSurge(inFlight, surge db.Node, ranker db.ControlPlaneScaleDownRanker) bool {
	if ranker == nil || inFlight.ID == surge.ID || !isCleanJoinedControlPlaneMember(inFlight) {
		return false
	}
	return ranker.ScaleDownRank(inFlight) > ranker.ScaleDownRank(surge)
}

// clearRebalanceMarker clears the rebalance marker so the joined surge is an
// ordinary member. The surge keeps its pinned snapshot: the capped preference
// matters only while provisioning, and ranking uses the cluster's live config
// once joined.
func (r *reconciler) clearRebalanceMarker(ctx context.Context, surge db.Node, logger *slog.Logger) error {
	locked, err := r.db.GetNodeForUpdate(ctx, surge.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("locking rebalance surge node %s: %w", surge.ID, err)
	}
	locked.Conditions.ControlPlanePreferredReplacement = db.NewCondition(false, "Rebalance surge joined")
	if _, err := r.db.UpdateNode(ctx, &locked); err != nil {
		return fmt.Errorf("finishing rebalance surge for node %s: %w", locked.ID, err)
	}
	logger.Info("Rebalance surge joined; cleared marker (surge keeps its capped snapshot)")
	return nil
}

// leastPreferredControlPlaneNodeToShed returns the node a joined surge replaces:
// the least-preferred joined member other than the surge, and only when it ranks
// strictly worse. No victim means the surge is not an improvement on the members
// that are already joined.
func leastPreferredControlPlaneNodeToShed(cp *controlPlane, surge db.Node, ranker db.ControlPlaneScaleDownRanker) (db.Node, bool) {
	if ranker == nil {
		return db.Node{}, false
	}
	var candidates []db.Node
	for _, node := range cp.scaleDownEligible() {
		if node.ID != surge.ID {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return db.Node{}, false
	}
	victim := leastPreferredControlPlaneNode(candidates, ranker)
	if ranker.ScaleDownRank(victim) <= ranker.ScaleDownRank(surge) {
		return db.Node{}, false
	}
	return victim, true
}

// abandonRebalanceSurge tears the surge down through the serialized teardown
// path, the same one scale-down and rollout use. It reports whether this pass
// started the teardown: teardownOldNode is a no-op when a delete is already in
// flight for the surge, and the caller must not then short-circuit the reconcile,
// so that reconcileRequestedDeletes can promote the already-recorded delete.
func (r *reconciler) abandonRebalanceSurge(ctx context.Context, cp *controlPlane, surge db.Node, reason string, logger *slog.Logger) (bool, error) {
	started, err := r.teardownOldNode(ctx, cp, surge, "Rebalance surge abandoned: "+reason, "rebalance-abandon", logger)
	if err != nil {
		return false, fmt.Errorf("abandoning rebalance surge for node %s: %w", surge.ID, err)
	}
	if started {
		logger.Info("Abandoned control-plane rebalance surge", "reason", reason)
	}
	return started, nil
}

// rebalanceSurgeExpired reports whether a not-yet-joined surge has been coming up
// longer than defaultRebalanceSurgeTimeout, anchored on when it entered Pending
// (falling back to its creation time).
func rebalanceSurgeExpired(surge db.Node) bool {
	anchor := surge.Conditions.Pending.LastTransitionTime
	if anchor.IsZero() {
		anchor = surge.CreatedAt
	}
	return !anchor.IsZero() && time.Since(anchor) > defaultRebalanceSurgeTimeout
}

// worstControlPlaneRank returns the highest ScaleDownRank among the eligible
// nodes -- the exclusive bound for a surge's fallback. Callers gate on
// controlPlaneHasNodeBelowPrimaryRank first, so it is at least
// primaryControlPlaneRank+1.
func worstControlPlaneRank(eligible []db.Node, ranker db.ControlPlaneScaleDownRanker) int {
	worst := primaryControlPlaneRank
	for _, node := range eligible {
		if rank := ranker.ScaleDownRank(node); rank > worst {
			worst = rank
		}
	}
	return worst
}
