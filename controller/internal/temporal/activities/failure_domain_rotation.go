// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/log"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/failuredomains"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
)

// rotateFailureDomain moves a pending control-plane node off a failure domain the
// provider has reported out of capacity for the configured grace period, and marks
// that domain unplaceable for the cluster. It patches the Machine first and the
// node row second; a poll that finds them disagreeing finishes the row update.
// Rebalance surges are left alone. It returns whether it moved the node.
func (a *Activities) rotateFailureDomain(
	ctx context.Context,
	logger log.Logger,
	cluster db.Cluster,
	node db.Node,
	machine *clusterv1.Machine,
	unavailable bool,
) (bool, error) {
	current := machine.Spec.FailureDomain
	if !a.cfg.ControlPlaneFailureDomainFallbackEnabled || !node.ControlPlane || current == "" {
		return false, nil
	}
	// A surge is placed in the domain of the node it replaces; moving it can leave
	// two members in one domain. The rebalance step abandons it on its own timeout.
	if node.Conditions.ControlPlanePreferredReplacement.Status == db.ConditionStatusTrue {
		return false, nil
	}

	if node.FailureDomain != current {
		return false, a.recordFailureDomainMove(ctx, node, current, appendTried(node.FailureDomainsTried, node.FailureDomain))
	}

	switch {
	case !unavailable:
		if node.FailureDomainUnavailableSince != nil {
			return false, a.setFailureDomainUnavailableSince(ctx, node, nil)
		}
		return false, nil
	case node.FailureDomainUnavailableSince == nil:
		now := time.Now()
		return false, a.setFailureDomainUnavailableSince(ctx, node, &now)
	case time.Since(*node.FailureDomainUnavailableSince) < a.cfg.ControlPlaneFailureDomainFallbackGracePeriod:
		return false, nil
	}

	next, tried, err := a.nextFailureDomain(ctx, cluster, node, current)
	if err != nil || next == "" {
		return false, err
	}

	patch := client.MergeFromWithOptions(machine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	machine.Spec.FailureDomain = next
	if err := a.k8sClient.Patch(ctx, machine, patch); err != nil {
		return false, fmt.Errorf("moving CAPI machine %s from failure domain %q to %q: %w", machine.Name, current, next, err)
	}
	if err := a.recordFailureDomainMove(ctx, node, next, tried); err != nil {
		return false, err
	}

	if err := a.markFailureDomainUnplaceable(ctx, cluster.ID, current); err != nil {
		logger.Warn("Could not mark the failure domain unplaceable; later placements may choose it",
			"clusterID", cluster.ID, "failureDomain", current, "error", err)
	}

	metrics.RecordControlPlaneFailureDomainRotation(cluster.ID.String(), cluster.Name)
	logger.Info("Moved pending control-plane node to another failure domain",
		"clusterID", cluster.ID, "nodeID", node.ID, "nodeName", node.Name,
		"from", current, "to", next, "tried", tried,
		"unavailableSince", *node.FailureDomainUnavailableSince)
	return true, nil
}

// nextFailureDomain returns the least-used eligible domain the node has not
// tried, and the tried list to persist with the move. When every other domain
// has been tried the walk starts over. Domains the cluster marked unplaceable are
// chosen only when no other remains. It returns "" when no other domain exists.
func (a *Activities) nextFailureDomain(ctx context.Context, cluster db.Cluster, node db.Node, current string) (string, []string, error) {
	snapshot, err := failuredomains.Discover(ctx, a.k8sClient, a.cfg.ManagementNamespaceOrDefault(), cluster.Name)
	if err != nil {
		return "", nil, fmt.Errorf("discovering failure domains for cluster %s: %w", cluster.ID, err)
	}
	controlPlane := true
	cpNodes, err := a.db.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID, ControlPlane: &controlPlane})
	if err != nil {
		return "", nil, fmt.Errorf("listing control-plane nodes of cluster %s: %w", cluster.ID, err)
	}
	others := slices.DeleteFunc(cpNodes, func(n db.Node) bool { return n.ID == node.ID })

	var marked []string
	now := time.Now()
	for _, domain := range snapshot.Eligible {
		if domain != current && failuredomains.IsMarkedUnplaceable(cluster.Conditions.UnplaceableFailureDomains, domain, now) {
			marked = append(marked, domain)
		}
	}

	tried := appendTried(node.FailureDomainsTried, current)
	restart := []string{current}
	// Marked domains come last: probing one each grace period beats waiting out the mark.
	passes := []struct{ tried, exclude []string }{
		{tried, append(slices.Clone(tried), marked...)},
		{restart, append(slices.Clone(restart), marked...)},
		{tried, tried},
		{restart, restart},
	}
	for _, pass := range passes {
		allocator := failuredomains.NewControlPlaneAllocator(others, snapshot)
		allocator.Exclude(pass.exclude...)
		if next := allocator.Take(); next != "" {
			return next, pass.tried, nil
		}
	}
	return "", nil, nil
}

func (a *Activities) setFailureDomainUnavailableSince(ctx context.Context, node db.Node, since *time.Time) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		n, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("loading node %s: %w", node.ID, err)
		}
		n.FailureDomainUnavailableSince = since
		if _, err := tx.UpdateNode(ctx, &n); err != nil {
			return fmt.Errorf("updating failure-domain unavailable time of node %s: %w", node.ID, err)
		}
		return nil
	})
}

// markFailureDomainUnplaceable stamps domain in the cluster's unplaceable failure
// domains under the cluster row lock, so every placement path avoids it for
// failuredomains.UnplaceableTTL.
func (a *Activities) markFailureDomainUnplaceable(ctx context.Context, clusterID uuid.UUID, domain string) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, clusterID)
		if err != nil {
			return fmt.Errorf("loading cluster %s: %w", clusterID, err)
		}
		cluster.Conditions.UnplaceableFailureDomains = failuredomains.MarkUnplaceable(cluster.Conditions.UnplaceableFailureDomains, domain, time.Now())
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("marking failure domain %q unplaceable for cluster %s: %w", domain, clusterID, err)
		}
		return nil
	})
}

func (a *Activities) recordFailureDomainMove(ctx context.Context, node db.Node, domain string, tried []string) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		n, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("loading node %s: %w", node.ID, err)
		}
		n.FailureDomain = domain
		n.FailureDomainsTried = tried
		n.FailureDomainUnavailableSince = nil
		if _, err := tx.UpdateNode(ctx, &n); err != nil {
			return fmt.Errorf("recording failure-domain move of node %s: %w", node.ID, err)
		}
		return nil
	})
}

func appendTried(tried []string, domain string) []string {
	if domain == "" || slices.Contains(tried, domain) {
		return slices.Clone(tried)
	}
	return append(slices.Clone(tried), domain)
}

// failureDomainWaitNote describes the domain a pending control-plane node waits on
// and the domains it has left, or "" when it is not waiting on a domain.
func failureDomainWaitNote(node db.Node) string {
	if node.FailureDomainUnavailableSince == nil && len(node.FailureDomainsTried) == 0 {
		return ""
	}
	note := fmt.Sprintf("waiting on failure domain %q", node.FailureDomain)
	if len(node.FailureDomainsTried) > 0 {
		note += "; tried " + strings.Join(node.FailureDomainsTried, ", ")
	}
	return note
}
