// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"math"

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
	machinesPerDomain map[string]int
	selection         failureDomainSelectionAlgorithm
}

// failureDomainPlacement owns the allocator shared by node-creation paths in a
// reconcile and tracks whether failure-domain-dependent work was deferred
// because the allocator was unavailable.
// An available allocator with no domains is valid: it hands out "" for a
// provider that does not advertise failure-domain support.
type failureDomainPlacement struct {
	allocator *domainAllocator
	deferred  bool
}

func newFailureDomainPlacement(allocator *domainAllocator) *failureDomainPlacement {
	return &failureDomainPlacement{allocator: allocator}
}

// take gets the next failure domain placement accordingly to the allocator's
// selection algorithm.
func (p *failureDomainPlacement) take() *string {
	return new(p.allocator.take())
}

func (p *failureDomainPlacement) markDeferred() {
	p.deferred = true
}

func (p *failureDomainPlacement) wasDeferred() bool {
	return p.deferred
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
	return &domainAllocator{machinesPerDomain: counts, selection: selection}
}

// record counts a placement that already exists. Domains that are no longer
// eligible (i.e. not in cluster.status.failureDomains or flagged with controlPlane: false)
// are ignored, so they don't skew new placements.
func (a *domainAllocator) record(domain string) {
	if _, eligible := a.machinesPerDomain[domain]; eligible {
		a.machinesPerDomain[domain]++
	}
}

// take returns the next placement and counts it.
func (a *domainAllocator) take() string {
	if len(a.machinesPerDomain) == 0 {
		return ""
	}
	selected := a.selection(a.machinesPerDomain)
	a.machinesPerDomain[selected]++
	return selected
}

// prepareFailureDomainPlacement backfills decisions for nodes that already
// exist when failure-domain support is first deployed and seeds the allocator
// used by new nodes. Every block labeled "Migration only" can be removed once
// every control-plane node is guaranteed to have a non-nil failure_domain.
func (r *reconciler) prepareFailureDomainPlacement(
	ctx context.Context,
	nodes []db.Node,
	failureDomains *failureDomains,
) (*failureDomainPlacement, error) {
	placement := newFailureDomainPlacement(nil)
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return placement, nil
	}

	// Discovery must succeed before new nodes can be placed.
	if failureDomains == nil {
		// Migration only: an existing row may still be pending without a decision
		// and needs a timed retry when discovery fails.
		for i := range nodes {
			node := &nodes[i]
			if !node.IsMarkedForDeletion() && !node.HasInfrastructureProvisioningStarted() && !node.HasFailureDomainDecision() {
				// Mark deferred only if discovery failed and a node needs placement.
				placement.markDeferred()
				break
			}
		}
		return placement, nil
	}

	allocator := newDomainAllocator(leastUsedFailureDomainSelection, failureDomains.eligible)
	placement.allocator = allocator

	// An empty allocator remains available and hands out "" to new nodes.
	if len(failureDomains.eligible) == 0 {
		// Migration only: backfill existing rows for a provider without domains.
		if err := r.recordNoFailureDomain(ctx, nodes); err != nil {
			return nil, err
		}
		return placement, nil
	}

	// Persisted decisions are steady-state input to the allocator.
	// Migration only: migration 20260915120000 adds failure_domain as nullable,
	// so these track existing rows whose decisions still need to be adopted,
	// observed, or assigned.
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
		case failureDomains.observedByNodeName[node.Name] != "":
			// Migration only: adopt the placement CAPI already reports for an
			// existing row that has no persisted decision.
			node.FailureDomain = new(failureDomains.observedByNodeName[node.Name])
			allocator.record(*node.FailureDomain)
			if _, err := r.db.UpdateNode(ctx, node); err != nil {
				return nil, fmt.Errorf("adopting observed failure domain for node %s: %w", node.ID, err)
			}
		case node.HasInfrastructureProvisioningStarted():
			// Migration only: wait for CAPI to report the placement of existing
			// infrastructure rather than making a new decision for it.
			hasUnobserved = true
		default:
			// Migration only: existing pending, unclaimed nodes are still safe
			// to place directly.
			unplaced = append(unplaced, node)
		}
	}

	// Migration only: the allocator remains unavailable until every existing
	// infrastructure placement is known; otherwise new placements would use
	// incomplete counts.
	if hasUnobserved {
		placement.allocator = nil
		if len(unplaced) > 0 {
			placement.markDeferred()
		}
		return placement, nil
	}

	// Migration only: persist decisions for nodes that were pending and unclaimed
	// when this feature was deployed.
	for _, node := range unplaced {
		node.FailureDomain = new(allocator.take())
		if _, err := r.db.UpdateNode(ctx, node); err != nil {
			return nil, fmt.Errorf("persisting failure domain for node %s: %w", node.ID, err)
		}
	}
	return placement, nil
}

// recordNoFailureDomain is migration-only backfill for undecided nodes on a
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
