// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

const (
	// stuckPendingNodeTimeout bounds how long a domain-pinned control-plane node the
	// provider reports as out of capacity may wait before it is re-placed.
	stuckPendingNodeTimeout = 15 * time.Minute

	// unplaceableDomainTTL is how long placement avoids a domain after a node
	// pinned there was given up on.
	unplaceableDomainTTL = 3 * time.Hour

	stuckPendingReplaceReason = "Control-plane node pending past the timeout with its failure domain out of capacity: re-placing in another failure domain"
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

// isMarkedUnplaceable reports whether domain was marked unplaceable within unplaceableDomainTTL of now.
func isMarkedUnplaceable(unplaceable map[string]time.Time, domain string, now time.Time) bool {
	marked, ok := unplaceable[domain]
	return ok && now.Sub(marked) < unplaceableDomainTTL
}

// placeableDomains drops domains the controller recently gave up on. When that
// would leave none, it returns every eligible domain so placement still proceeds.
func placeableDomains(eligible []string, unplaceable map[string]time.Time, now time.Time) []string {
	placeable := make([]string, 0, len(eligible))
	for _, domain := range eligible {
		if isMarkedUnplaceable(unplaceable, domain, now) {
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
// stuck pending in a failure domain the provider reports out of capacity, and marks
// that domain unplaceable so scale-up recreates the node elsewhere. It reports
// whether it recorded a delete, in which case the caller ends the pass because the
// snapshot does not reflect it.
func (r *reconciler) reconcileStuckPendingNodes(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	machines clients.MachineMaterialization,
) (bool, error) {
	if cp.hasDeleteInFlight() || r.controlPlaneReplacementBlocked(cluster) {
		return false, nil
	}

	var stuck []db.Node
	for _, node := range cp.nodes {
		if isStuckPendingInDomain(node) && r.failureDomainUnavailable(ctx, machines, node) {
			stuck = append(stuck, node)
		}
	}
	if len(stuck) == 0 {
		return false, nil
	}
	victim := leastPreferredControlPlaneNode(stuck, nil)

	logger := r.logger.With("clusterID", cluster.ID, "clusterName", cluster.Name, "nodeID", victim.ID, "nodeName", victim.Name,
		"failureDomain", victim.FailureDomain)

	// Re-placing helps only when another domain can take the node; with every
	// other domain marked or absent the replacement would land here again.
	if !hasAlternativeDomain(cp.failureDomains, victim.FailureDomain, cluster.Conditions.UnplaceableFailureDomains, time.Now()) {
		logger.Debug("Control-plane node stuck pending but no other failure domain is placeable; leaving it")
		return false, nil
	}

	recorded, err := r.recordControlPlaneManagedDelete(ctx, logger, victim, stuckPendingReplaceReason)
	if err != nil || !recorded {
		return false, err
	}
	if err := r.markDomainUnplaceable(ctx, cluster.ID, victim.FailureDomain, logger); err != nil {
		return false, err
	}
	logger.Info("Recorded delete of a control-plane node stuck pending in its failure domain")
	return true, nil
}

// failureDomainUnavailable reports whether the provider reports node's requested
// failure domain out of capacity. A failed lookup reports false, leaving the node in place.
func (r *reconciler) failureDomainUnavailable(ctx context.Context, machines clients.MachineMaterialization, node db.Node) bool {
	logger := r.logger.With("nodeID", node.ID, "nodeName", node.Name, "failureDomain", node.FailureDomain)
	machine := &clusterv1.Machine{}
	if err := r.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: r.cfg.ManagementNamespaceOrDefault(), Name: node.Name}, machine); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Warn("Could not read the CAPI Machine of a pending control-plane node", "error", err)
		}
		return false
	}
	unavailable, err := machines.FailureDomainUnavailable(ctx, shared.MachineAdvanceDeps{K8sClient: r.k8sClient}, machine)
	if err != nil {
		logger.Warn("Could not read the placement failure of a pending control-plane node", "error", err)
		return false
	}
	return unavailable
}

// hasAlternativeDomain reports whether an eligible domain other than domain is
// not marked unplaceable.
func hasAlternativeDomain(f *failureDomains, domain string, unplaceable map[string]time.Time, now time.Time) bool {
	if f == nil {
		return false
	}
	for _, candidate := range f.eligible {
		if candidate != domain && !isMarkedUnplaceable(unplaceable, candidate, now) {
			return true
		}
	}
	return false
}

// markDomainUnplaceable stamps the domain and prunes expired entries. The
// controller's reconcile transaction already holds the cluster row lock; it re-reads
// the cluster so it extends conditions written earlier in the same reconcile.
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
