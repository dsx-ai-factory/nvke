// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

// reconcileControlPlaneScaleDown records a delete when the desired count drops
// below the healthy node count. It only records intent; reconcileRequestedDeletes
// promotes it one node at a time, draining through the same serialized path as
// any other control-plane delete. It runs last, after scale-up, remediation and
// rollout have grown, so the control plane never shrinks while it owes a replacement.
func (r *reconciler) reconcileControlPlaneScaleDown(ctx context.Context, cluster db.Cluster, cp *controlPlane) error {
	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"desiredCount", cluster.ControlPlaneConfig.DesiredCount,
	)

	// An operator can switch off automatic scale-down per cluster. The surplus
	// then stays until the flag is cleared or the nodes are removed another way.
	// This gates new scale-down decisions only: a delete recorded before the
	// flag was set is owned by reconcileRequestedDeletes and still completes
	// through the serialized teardown path, which the quorum floor bounds. Record
	// and promotion happen in the same reconcile, so that window is a single loop.
	if cluster.ControlPlaneConfig.AutoScaleDownDisabled {
		logger.Debug("Control-plane auto scale-down disabled; skipping")
		return nil
	}

	// Don't fight an active rollout for the same slots. Drift with recycling
	// disabled is not a rollout, so it does not block.
	if cp.upgradeInProgress(cluster) {
		return nil
	}

	// Surplus is over the controller-managed nodes this shrink may remove, which
	// counts toward desiredCount the same way scale-up does (scale-up grows on the
	// full node count, not the joined subset). A recorded delete drops out of the
	// set next reconcile, so the loop paces the shrink one node at a time.
	removable := cp.scaleDownRemovable()
	surplus := len(removable) - cluster.ControlPlaneConfig.DesiredCount
	if surplus <= 0 {
		return nil
	}

	// Shed a not-yet-joined surplus node first. Scale-up already counted it toward
	// desiredCount, it is not an etcd member, and tearing it down disrupts no
	// running member -- so it is the cheapest shed and the one that keeps
	// desiredCount converging when the surplus is a node still coming up. The
	// quorum floor does not apply to it (ensureControlPlaneQuorumFloor exempts a
	// node that never joined), so this path skips the floor pre-check.
	eligible := cp.scaleDownEligible()
	if victim, ok := selectNotYetJoinedToScaleDown(removable, eligible); ok {
		return r.recordControlPlaneScaleDownDelete(ctx, logger, victim, surplus)
	}

	// Every removable node is a joined member: never shrink below the
	// fault-tolerant floor, even when desired is lower. The delete path's
	// ensureControlPlaneQuorumFloor is authoritative; this pre-check just avoids
	// selecting a victim it would refuse. A desiredCount below the floor is a
	// steady state that never converges, so this logs at debug rather than warning
	// on every reconcile.
	if len(eligible)-1 < nodes.MinFaultTolerantControlPlaneNodes {
		logger.Debug("Control-plane scale-down held at the fault-tolerant floor",
			"eligible", len(eligible),
			"minFaultTolerant", nodes.MinFaultTolerantControlPlaneNodes,
		)
		return nil
	}

	victim := selectControlPlaneNodeToScaleDown(eligible, cluster.ControlPlaneConfig.ScaleDownRanker())
	return r.recordControlPlaneScaleDownDelete(ctx, logger, victim, surplus)
}

// recordControlPlaneScaleDownDelete records a scale-down delete as intent, which
// reconcileRequestedDeletes later promotes to a teardown one node at a time.
// Unlike node-replacing callers, scale-down does not skip the quorum floor: a
// shrink has no replacement coming, so the record-time floor stays authoritative
// (and harmlessly exempts a not-yet-joined victim, which is not an etcd member).
func (r *reconciler) recordControlPlaneScaleDownDelete(ctx context.Context, logger *slog.Logger, victim db.Node, surplus int) error {
	nodeLogger := logger.With("nodeID", victim.ID, "nodeName", victim.Name)

	if err := nodes.DeleteWithTx(ctx, r.db, victim.ID,
		nodes.WithReason("Control-plane scale-down: desired count decreased"),
		nodes.WithAllowCPNode(true),
		// Preconditions are enforced above; skip guardLiveNodeDelete's cluster-row
		// lock and its live-API deletion policy. The node row is still read FOR
		// UPDATE, and the record-time quorum floor still applies.
		nodes.WithSkipLiveDeleteGuard(),
	); err != nil {
		// The record-time floor may still refuse if the live count is tighter
		// than the snapshot -- that is the guard working, not a failure.
		var floorErr *nodes.ControlPlaneQuorumFloorError
		if errors.As(err, &floorErr) {
			nodeLogger.Warn("Control-plane scale-down refused by the quorum floor at record time", "err", err)
			return nil
		}
		return fmt.Errorf("recording control-plane scale-down delete for node %s (%s): %w", victim.ID, victim.Name, err)
	}

	nodeLogger.Info("Recorded control-plane scale-down delete; teardown will start one node at a time",
		"surplus", surplus,
	)
	return nil
}

// upgradeInProgress reports whether the control plane is in an active upgrade:
// a Kubernetes version upgrade, recycling in flight, or recycling enabled with
// drift left to roll. Drift alone with recycling disabled is not a rollout, so
// it does not block.
func (c *controlPlane) upgradeInProgress(cluster db.Cluster) bool {
	if cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
		return true
	}
	if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
		return true
	}
	return cluster.EnableCPNodeRecycling && c.countNodesNeedingRollout() > 0
}

// scaleDownRemovable returns the controller-managed nodes a shrink may remove:
// everything except nodes already on their way out or in for replacement. It is
// the superset of scaleDownEligible that also includes not-yet-joined nodes, so
// surplus is counted the same way scale-up counts toward desiredCount -- without
// it a not-yet-joined surplus node would be invisible to both and never shed.
func (c *controlPlane) scaleDownRemovable() []db.Node {
	var removable []db.Node
	for _, node := range c.nodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		if nodes.IsDeleteInFlight(node) || isMarkedForRecycling(node) {
			continue
		}
		removable = append(removable, node)
	}
	return removable
}

// selectNotYetJoinedToScaleDown picks a removable node that is not yet a clean
// joined member -- still pending, provisioning, or unhealthy -- preferring the
// oldest then the lowest ID for a stable choice. These nodes are not etcd
// members, so they are shed ahead of joined members and outside the quorum
// floor. The bool is false when every removable node has joined.
func selectNotYetJoinedToScaleDown(removable, eligible []db.Node) (db.Node, bool) {
	joined := make(map[uuid.UUID]struct{}, len(eligible))
	for _, node := range eligible {
		joined[node.ID] = struct{}{}
	}

	var candidates []db.Node
	for _, node := range removable {
		if _, ok := joined[node.ID]; !ok {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return db.Node{}, false
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) // oldest first
		}
		return candidates[i].ID.String() < candidates[j].ID.String()
	})
	return candidates[0], true
}

// scaleDownEligible returns the controller-managed nodes safe to remove for a
// shrink: healthy, joined, and not already on their way out or in for
// replacement. Basing surplus on this set makes it converge one node at a time.
func (c *controlPlane) scaleDownEligible() []db.Node {
	var eligible []db.Node
	for _, node := range c.nodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		// On its way out or in for replacement: not removable surplus.
		if nodes.IsDeleteInFlight(node) || isMarkedForRecycling(node) {
			continue
		}
		// Only a fully healthy, joined member is a clean removal.
		if node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}
		if isPending(node) || !isProvisioned(node) || !isKubernetesReady(node) || !isNodeAgentReady(node) {
			continue
		}
		eligible = append(eligible, node)
	}
	return eligible
}

// primaryControlPlaneRank is the ScaleDownRank of a node on the primary
// (most-preferred) type; the ranker contract fixes it at 0.
const (
	primaryControlPlaneRank = 0
)

// controlPlaneHasNodeBelowPrimaryRank reports whether any eligible node sits
// below the primary type (ScaleDownRank above primary). It is the steady-state
// rebalance probe; a nil ranker never reports a candidate.
//
// Rebalance follow-up: at steady state (desired == count) the reconciler can use
// this to trigger a surge-then-shed rebalance, reusing the existing scale-up and
// scale-down paths -- surge a preferred replacement (only when the scheduler
// confirms a strictly better-ranked placement, to avoid churn), then let
// scale-down shed the least-preferred node once it joins.
func controlPlaneHasNodeBelowPrimaryRank(eligible []db.Node, ranker db.ControlPlaneScaleDownRanker) bool {
	if ranker == nil {
		return false
	}
	for _, node := range eligible {
		if ranker.ScaleDownRank(node) > primaryControlPlaneRank {
			return true
		}
	}
	return false
}

// selectControlPlaneNodeToScaleDown picks the surplus node to remove: least
// preferred first (higher ScaleDownRank), then oldest, then lowest ID for a
// stable choice. A nil ranker collapses this to oldest-then-ID. Failure-domain
// spreading is out of scope; the quorum floor already bounds the shrink.
func selectControlPlaneNodeToScaleDown(eligible []db.Node, ranker db.ControlPlaneScaleDownRanker) db.Node {
	rankOf := func(node db.Node) int {
		if ranker == nil {
			return 0
		}
		return ranker.ScaleDownRank(node)
	}

	sorted := make([]db.Node, len(eligible))
	copy(sorted, eligible)
	sort.Slice(sorted, func(i, j int) bool {
		if ri, rj := rankOf(sorted[i]), rankOf(sorted[j]); ri != rj {
			return ri > rj // least preferred (higher rank) first
		}
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) // oldest first
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	return sorted[0]
}
