// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"

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

	// Don't fight an active rollout for the same slots. Drift with recycling
	// disabled is not a rollout, so it does not block.
	if cp.upgradeInProgress(cluster) {
		return nil
	}

	// Surplus is over eligible (healthy, joined) nodes only, so a recorded delete
	// drops out next reconcile and the loop paces itself to one at a time.
	eligible := cp.scaleDownEligible()
	surplus := len(eligible) - cluster.ControlPlaneConfig.DesiredCount
	if surplus <= 0 {
		return nil
	}

	// Never shrink below the fault-tolerant floor, even when desired is lower.
	// The delete path's ensureControlPlaneQuorumFloor is authoritative; this
	// pre-check just avoids selecting a victim it would refuse.
	if len(eligible)-1 < nodes.MinFaultTolerantControlPlaneNodes {
		logger.Warn("Control-plane scale-down held at the fault-tolerant floor",
			"eligible", len(eligible),
			"minFaultTolerant", nodes.MinFaultTolerantControlPlaneNodes,
		)
		return nil
	}

	victim := selectControlPlaneNodeToScaleDown(eligible, cluster.ControlPlaneConfig.ScaleDownRanker())
	nodeLogger := logger.With("nodeID", victim.ID, "nodeName", victim.Name)

	// Record intent and let reconcileRequestedDeletes start the teardown. The
	// quorum floor is NOT skipped -- this is a real shrink -- so the record-time
	// floor stays authoritative. DeleteWithTx records CP nodes as intent rather
	// than tearing them down directly.
	if err := nodes.DeleteWithTx(ctx, r.db, victim.ID,
		nodes.WithReason("Control-plane scale-down: desired count decreased"),
		nodes.WithAllowCPNode(true),
		// Preconditions are enforced above; skip the live-API guard (also avoids
		// SELECT FOR UPDATE, which SQLite unit tests cannot run).
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
		"eligible", len(eligible),
		"surplus", surplus,
	)
	return nil
}

// upgradeInProgress reports whether the control plane is actively replacing
// nodes: recycling in flight, or recycling enabled with drift left to roll.
// Drift alone with recycling disabled is not a rollout, so it does not block.
func (c *controlPlane) upgradeInProgress(cluster db.Cluster) bool {
	if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
		return true
	}
	return cluster.EnableCPNodeRecycling && c.countNodesNeedingRollout() > 0
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
// Rebalance follow-up (Refs NKX-15398): the reconciler uses this at steady state
// (desired == count) to trigger a surge-then-shed rebalance, reusing the existing
// scale-up and scale-down paths -- surge a preferred replacement (only when the
// scheduler confirms a strictly better-ranked placement, to avoid churn), then
// let scale-down shed the least-preferred node once it joins.
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
