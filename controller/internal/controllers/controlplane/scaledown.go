// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// The selector sheds any not-yet-joined surplus first, then the least-preferred
	// joined member. A not-yet-joined node is pure surplus scale-up already
	// counted, is not an etcd member, and disrupts nothing -- so it is the cheapest
	// shed and the one that keeps desiredCount converging when the surplus is still
	// coming up.
	victim := selectControlPlaneNodeToScaleDown(removable, cluster.ControlPlaneConfig.ScaleDownRanker())

	// Never shrink the joined control plane below the fault-tolerant floor. A
	// joined node counts toward quorum whether or not it is currently healthy --
	// its etcd member still exists -- so the count here mirrors the authoritative
	// delete-path floor (countSurvivingJoinedControlPlane), which counts joined
	// members regardless of health. The floor is applied only when the victim is
	// itself a healthy joined member, matching ensureControlPlaneQuorumFloor, which
	// exempts a not-yet-joined or unhealthy victim -- removing a dead member is how
	// a replacement starts, so the floor must not block it. That delete-path floor
	// is authoritative; this pre-check just avoids selecting a victim it would
	// refuse and keeps the two consistent. A desiredCount below the floor is a
	// steady state that never converges, so it logs at debug rather than warning on
	// every reconcile.
	if isCleanJoinedControlPlaneMember(victim) {
		joinedMembers := 0
		for _, node := range removable {
			if node.Conditions.Joined.Status == db.ConditionStatusTrue {
				joinedMembers++
			}
		}
		if joinedMembers <= nodes.MinFaultTolerantControlPlaneNodes {
			logger.Debug("Control-plane scale-down held at the fault-tolerant floor",
				"joinedMembers", joinedMembers,
				"minFaultTolerant", nodes.MinFaultTolerantControlPlaneNodes,
			)
			return nil
		}
	}

	return r.recordControlPlaneScaleDownDelete(ctx, logger, victim, surplus)
}

// recordControlPlaneScaleDownDelete records a scale-down delete as intent, which
// reconcileRequestedDeletes later promotes to a teardown one node at a time.
// Unlike node-replacing callers, scale-down does not skip the quorum floor: a
// shrink has no replacement coming, so the record-time floor stays authoritative
// (and harmlessly exempts a not-yet-joined victim, which is not an etcd member).
func (r *reconciler) recordControlPlaneScaleDownDelete(ctx context.Context, logger *slog.Logger, victim db.Node, surplus int) error {
	recorded, err := r.recordControlPlaneManagedDelete(ctx, logger, victim, "Control-plane scale-down: desired count decreased")
	if err != nil {
		return err
	}
	if recorded {
		logger.With("nodeID", victim.ID, "nodeName", victim.Name).Info(
			"Recorded control-plane scale-down delete; teardown will start one node at a time",
			"surplus", surplus,
		)
	}
	return nil
}

// recordControlPlaneManagedDelete records a controller-initiated delete of a
// control-plane node through the shared serialized teardown path, and reports
// whether the delete was recorded. Automatic scale-down and steady-state
// rebalance both shed a surplus node this way, so the record-time quorum floor
// bounds either. A floor refusal means the live count is tighter than the
// snapshot -- the guard working, not a failure -- so it logs and returns
// (false, nil) rather than erroring the reconcile.
func (r *reconciler) recordControlPlaneManagedDelete(ctx context.Context, logger *slog.Logger, victim db.Node, reason string) (bool, error) {
	nodeLogger := logger.With("nodeID", victim.ID, "nodeName", victim.Name)

	if err := nodes.DeleteWithTx(ctx, r.db, victim.ID,
		nodes.WithReason(reason),
		nodes.WithAllowCPNode(true),
		// Preconditions are enforced by the caller; skip guardLiveNodeDelete's
		// live-API deletion policy. The skip path still locks the cluster row and
		// refuses a blocked control-plane mutation, reads the node row FOR UPDATE,
		// and applies the record-time quorum floor.
		nodes.WithSkipLiveDeleteGuard(),
	); err != nil {
		var floorErr *nodes.ControlPlaneQuorumFloorError
		if errors.As(err, &floorErr) {
			nodeLogger.Warn("Control-plane delete refused by the quorum floor at record time", "err", err)
			return false, nil
		}
		// A delete already recorded for this node is the desired end state, not a
		// failure, so treat it as a benign no-op. Both callers -- steady-state
		// rebalance and automatic scale-down -- select victims from sets that
		// exclude a node with a delete already in flight, and the reconcile returns
		// after a rebalance shed before scale-down runs, so the two no longer record
		// the same node in one pass. This remains a defensive guard: erroring here
		// would roll back the whole transactional reconcile, undoing a delete that
		// was legitimately recorded and wedging the shrink in a livelock, so a
		// repeat record must stay a no-op.
		if errors.Is(err, nodes.ErrDeleteAlreadyRequested) {
			nodeLogger.Debug("Control-plane delete already recorded; treating as a no-op")
			return false, nil
		}
		return false, fmt.Errorf("recording control-plane delete for node %s (%s): %w", victim.ID, victim.Name, err)
	}

	return true, nil
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
//
// A rebalance surge node is held out while it is coming up: it must neither count
// toward surplus (its +1 would otherwise shrink a healthy node before the
// replacement is ready) nor be selected as a victim (shedding it would undo the
// rebalance). Because surplus and victim both derive from this set, holding it
// out here covers both. The hold is self-limiting -- once the surge is a clean
// joined member the rebalance step sheds the old, less-preferred node itself (not
// scale-down) and clears the marker, after which the former surge is an ordinary
// member at rank 0.
func (c *controlPlane) scaleDownRemovable() []db.Node {
	var removable []db.Node
	for _, node := range c.nodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		if nodes.IsDeleteInFlight(node) || isMarkedForRecycling(node) {
			continue
		}
		if isRebalanceReplacement(node) && !isCleanJoinedControlPlaneMember(node) {
			continue
		}
		removable = append(removable, node)
	}
	return removable
}

// isCleanJoinedControlPlaneMember reports whether a node is a fully healthy,
// joined control-plane member: the shape the quorum floor protects, and the one
// scale-down sheds only after any not-yet-joined surplus. Everything else --
// pending, provisioning, or unhealthy -- is not an etcd member, so it is shed
// first and outside the floor.
func isCleanJoinedControlPlaneMember(node db.Node) bool {
	if node.Conditions.Joined.Status != db.ConditionStatusTrue {
		return false
	}
	return !isPending(node) && isProvisioned(node) && isKubernetesReady(node) && isNodeAgentReady(node)
}

// scaleDownEligible returns the controller-managed nodes safe to remove for a
// shrink: healthy, joined, and not already on their way out or in for
// replacement. It is the joined-member subset of scaleDownRemovable.
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
		if !isCleanJoinedControlPlaneMember(node) {
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
// rebalance probe reconcileControlPlaneRebalance gates on: when it reports a
// candidate, the rebalance step surges a more-preferred replacement and, once the
// replacement joins, sheds the least-preferred node. A nil ranker never reports a
// candidate, so a provider with no ordered preference is never rebalanced.
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

// selectControlPlaneNodeToScaleDown picks the surplus node to remove from the
// removable set: a not-yet-joined node first (pure surplus, not an etcd member),
// then the least-preferred node. A nil ranker collapses the rank tie-break to
// oldest-then-ID. Failure-domain spreading is out of scope; the quorum floor
// already bounds the shrink.
func selectControlPlaneNodeToScaleDown(removable []db.Node, ranker db.ControlPlaneScaleDownRanker) db.Node {
	var notJoined []db.Node
	for _, node := range removable {
		if !isCleanJoinedControlPlaneMember(node) {
			notJoined = append(notJoined, node)
		}
	}
	if len(notJoined) > 0 {
		return leastPreferredControlPlaneNode(notJoined, ranker)
	}
	return leastPreferredControlPlaneNode(removable, ranker)
}

// leastPreferredControlPlaneNode returns the least-preferred node: highest
// ScaleDownRank (0 is the most preferred type), then oldest, then lowest ID.
// A nil ranker collapses the rank to oldest-then-ID. nodes must be non-empty.
func leastPreferredControlPlaneNode(nodes []db.Node, ranker db.ControlPlaneScaleDownRanker) db.Node {
	rankOf := func(node db.Node) int {
		if ranker == nil {
			return 0
		}
		return ranker.ScaleDownRank(node)
	}

	sorted := make([]db.Node, len(nodes))
	copy(sorted, nodes)
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
