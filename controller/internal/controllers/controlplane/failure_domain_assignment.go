// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// failureDomainSelectionAlgorithm picks the next failure domain from the
// current placement count per eligible domain.
//
// It is called only with a non-empty map, must be deterministic, and must not
// modify the map. Counting the returned domain belongs to the caller.
type failureDomainSelectionAlgorithm func(counts map[string]int) string

// leastUsedFailureDomainSelection returns the domain with the fewest
// placements, breaking ties by domain name.
func leastUsedFailureDomainSelection(counts map[string]int) string {
	selected, fewest := "", math.MaxInt
	for domain, count := range counts {
		if count < fewest || (count == fewest && domain < selected) {
			selected, fewest = domain, count
		}
	}
	return selected
}

// domainAllocator hands out control-plane failure domains, keeping the count per
// domain balanced. It owns the counting; selection is the injected algorithm's.
// An allocator built from no domains hands out "", the placement for an
// infrastructure provider that does not advertise them.
type domainAllocator struct {
	counts    map[string]int
	selection failureDomainSelectionAlgorithm
}

// newDomainAllocator builds an allocator over the eligible domains. Unnamed domains are
// dropped: "" is reserved for a provider that advertises none, so no node may be
// placed in one.
func newDomainAllocator(selection failureDomainSelectionAlgorithm, eligible []string) *domainAllocator {
	counts := make(map[string]int, len(eligible))
	for _, domain := range eligible {
		if domain != "" {
			counts[domain] = 0
		}
	}
	return &domainAllocator{counts: counts, selection: selection}
}

// record counts a placement that already exists. Domains that are no longer
// eligible are ignored, so a retired domain cannot skew new placements.
func (a *domainAllocator) record(domain string) {
	if _, eligible := a.counts[domain]; eligible {
		a.counts[domain]++
	}
}

// take returns the next placement and counts it.
func (a *domainAllocator) take() string {
	if len(a.counts) == 0 {
		return ""
	}
	selected := a.selection(a.counts)
	a.counts[selected]++
	return selected
}

// assignFailureDomains records a failure domain on every control-plane node that
// does not have one.
//
// It returns true when the reconciler needs a requeue due to a failed discovery
// or a node whose infrastructure exists but whose placement CAPI has not reported.
func (r *reconciler) assignFailureDomains(ctx context.Context, clusterID uuid.UUID) (bool, error) {
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return false, nil
	}

	nodes, err := r.getControlPlaneNodes(ctx, clusterID)
	if err != nil {
		return false, err
	}

	// Provider advertises no domains, so there's nothing to assign.
	if r.failureDomainDiscoverySucceeded && len(r.failureDomains.eligible) == 0 {
		return false, r.recordNoFailureDomain(ctx, nodes)
	}

	allocator := newDomainAllocator(leastUsedFailureDomainSelection, r.failureDomains.eligible)

	// Both lack a recorded placement: unplaced nodes can still take one,
	// unobserved nodes already have infrastructure but the domain is unknown.
	var unplaced []*db.Node
	var hasUnobserved bool

	for i := range nodes {
		node := &nodes[i]
		switch {
		case node.IsMarkedForDeletion():
		case node.HasFailureDomainDecision():
			// Node record in db has a placement decision.
			// An empty string is a valid placement decision:
			// it's interpreted as belonging to a provider without
			// failure domain support.
			allocator.record(node.GetFailureDomain())
		case r.failureDomains.observedByNodeName[node.Name] != "":
			// Node record in db does not yet have a placement decision,
			// but its CAPI CR does have a failure domain set in the
			// status or spec (its "observed placement"),
			// so we update the node record in the db accordingly.
			node.FailureDomain = new(r.failureDomains.observedByNodeName[node.Name])
			allocator.record(*node.FailureDomain)
			if _, err := r.db.UpdateNode(ctx, node); err != nil {
				return false, fmt.Errorf("adopting observed failure domain for node %s: %w", node.ID, err)
			}
		case node.HasInfrastructureProvisioningStarted():
			// Nodes should not normally reach this state: the
			// scheduler starts ProvisionNode only after a failure-domain decision
			// has been persisted. This branch handles nodes that were already provisioning
			// when failure-domain assignment support was introduced.
			hasUnobserved = true
		default:
			unplaced = append(unplaced, node)
		}
	}

	if len(unplaced) == 0 {
		return false, nil
	}

	// In order to ensure a balanced topology, we can't proceed
	// until unobserved, existing nodes get a placement decision, nor can we
	// proceed with incomplete info on a failed discovery attempt.
	// In either case, return true here to signal a requeue to the caller.
	if hasUnobserved || !r.failureDomainDiscoverySucceeded {
		return true, nil
	}

	// We have what we need to assign a failure domain to
	// all pending nodes.
	for _, node := range unplaced {
		node.FailureDomain = new(allocator.take())
		if _, err := r.db.UpdateNode(ctx, node); err != nil {
			return false, fmt.Errorf("persisting failure domain for node %s: %w", node.ID, err)
		}
	}
	return false, nil
}

// recordNoFailureDomain marks every undecided node at "", the placement for a
// provider that advertises no domains.
func (r *reconciler) recordNoFailureDomain(ctx context.Context, nodes []db.Node) error {
	for i := range nodes {
		node := &nodes[i]
		if node.IsMarkedForDeletion() || node.HasFailureDomainDecision() {
			continue
		}
		node.FailureDomain = new("")
		if _, err := r.db.UpdateNode(ctx, node); err != nil {
			return fmt.Errorf("recording absent failure domain for node %s: %w", node.ID, err)
		}
	}
	return nil
}
