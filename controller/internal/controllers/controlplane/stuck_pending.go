// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

const (
	// stuckPendingNodeTimeout bounds how long a domain-pinned control-plane node
	// may wait for the provider to allocate an instance before it is re-placed.
	stuckPendingNodeTimeout = 45 * time.Minute

	// unplaceableDomainTTL is how long placement avoids a domain after a node
	// pinned there was given up on.
	unplaceableDomainTTL = 3 * time.Hour

	stuckPendingReplaceReason = "Control-plane node pending with no allocated instance past the timeout: re-placing in another failure domain"
)

// isStuckPendingInDomain reports whether a controller-managed control-plane node
// pinned to a failure domain has waited past the timeout without the provider
// allocating an instance. A rebalance surge is excluded because the rebalance
// step abandons it on its own timeout.
func isStuckPendingInDomain(node db.Node) bool {
	return node.IsManagedByControlPlaneController() &&
		node.FailureDomain != "" &&
		isPending(node) &&
		!node.HasAllocatedInstance() &&
		!isRebalanceReplacement(node) &&
		!nodes.IsDeleteInFlight(node) &&
		pendingLongerThan(node, stuckPendingNodeTimeout)
}

// placeableDomains drops domains the controller recently gave up on. When that
// would leave none, it returns every eligible domain so placement still proceeds.
func placeableDomains(eligible []string, unplaceable map[string]time.Time, now time.Time) []string {
	placeable := make([]string, 0, len(eligible))
	for _, domain := range eligible {
		if marked, ok := unplaceable[domain]; ok && now.Sub(marked) < unplaceableDomainTTL {
			continue
		}
		placeable = append(placeable, domain)
	}
	if len(placeable) == 0 {
		return eligible
	}
	return placeable
}

// reconcileStuckPendingNodes records a delete for the oldest control-plane node
// stuck pending in its failure domain and marks that domain unplaceable, so scale-up
// recreates the node elsewhere. It reports whether it recorded a delete, in which
// case the caller ends the pass because the snapshot does not reflect it.
func (r *reconciler) reconcileStuckPendingNodes(ctx context.Context, cluster db.Cluster, cp *controlPlane) (bool, error) {
	if cp.hasDeleteInFlight() || r.controlPlaneReplacementBlocked(cluster) {
		return false, nil
	}

	var stuck []db.Node
	for _, node := range cp.nodes {
		if isStuckPendingInDomain(node) {
			stuck = append(stuck, node)
		}
	}
	if len(stuck) == 0 {
		return false, nil
	}
	victim := leastPreferredControlPlaneNode(stuck, nil)

	logger := r.logger.With("clusterID", cluster.ID, "clusterName", cluster.Name, "nodeID", victim.ID, "nodeName", victim.Name,
		"failureDomain", victim.FailureDomain)

	if err := r.markDomainUnplaceable(ctx, cluster.ID, victim.FailureDomain, logger); err != nil {
		return false, err
	}
	recorded, err := r.recordControlPlaneManagedDelete(ctx, logger, victim, stuckPendingReplaceReason)
	if err != nil {
		return false, err
	}
	if recorded {
		logger.Info("Recorded delete of a control-plane node stuck pending in its failure domain")
	}
	return recorded, nil
}

// markDomainUnplaceable stamps the domain and prunes expired entries. It re-reads
// the cluster (the caller holds the lock) so it extends conditions written earlier
// in the same reconcile.
func (r *reconciler) markDomainUnplaceable(ctx context.Context, clusterID uuid.UUID, domain string, logger *slog.Logger) error {
	cluster, err := r.db.GetCluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("loading cluster %s to mark failure domain %q unplaceable: %w", clusterID, domain, err)
	}
	now := time.Now()
	marked := map[string]time.Time{domain: now}
	for d, at := range cluster.Conditions.UnplaceableFailureDomains {
		if d != domain && now.Sub(at) < unplaceableDomainTTL {
			marked[d] = at
		}
	}
	cluster.Conditions.UnplaceableFailureDomains = marked
	if _, err := r.db.UpdateCluster(ctx, &cluster); err != nil {
		return fmt.Errorf("marking failure domain %q unplaceable for cluster %s: %w", domain, clusterID, err)
	}
	logger.Debug("Marked failure domain unplaceable", "ttl", unplaceableDomainTTL)
	return nil
}
