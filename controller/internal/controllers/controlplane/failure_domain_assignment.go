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
// reconcile and records when one of those paths had to wait for domain data.
// An available allocator with no domains is valid: it hands out "" for a
// provider that does not advertise failure-domain support.
type failureDomainPlacement struct {
	allocator *domainAllocator
	deferred  bool
}

func newFailureDomainPlacement(allocator *domainAllocator) *failureDomainPlacement {
	return &failureDomainPlacement{allocator: allocator}
}

// take returns the next placement decision. Callers must first ensure that the
// allocator is available.
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
// eligible are ignored, so a retired domain cannot skew new placements.
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
// used by new nodes. The backfill paths can be removed once every control-plane
// node is guaranteed to have a non-nil failure_domain.
func (r *reconciler) prepareFailureDomainPlacement(
	ctx context.Context,
	nodes []db.Node,
	failureDomains *failureDomains,
) (*failureDomainPlacement, error) {
	placement := newFailureDomainPlacement(nil)
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return placement, nil
	}

	// Discovery must succeed before new nodes can be placed. The scan is a
	// first-deployment backfill case: rows created before failure-domain support
	// may still be pending without a decision and need a timed retry.
	if failureDomains == nil {
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
	// Existing undecided rows only need this update during the initial backfill.
	if len(failureDomains.eligible) == 0 {
		if err := r.recordNoFailureDomain(ctx, nodes); err != nil {
			return nil, err
		}
		return placement, nil
	}

	// Persisted decisions are steady-state input to the allocator. The other
	// branches are a backfill for migration 20260915120000, which adds
	// failure_domain as nullable: adopt placements already reported by CAPI, wait
	// when infrastructure exists but its placement is unknown, and place pending,
	// unclaimed nodes directly.
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
			// Node record in db does not yet have a placement decision,
			// but its CAPI CR does have a failure domain set in the
			// status or spec (its "observed placement"),
			// so we update the node record in the db accordingly.
			node.FailureDomain = new(failureDomains.observedByNodeName[node.Name])
			allocator.record(*node.FailureDomain)
			if _, err := r.db.UpdateNode(ctx, node); err != nil {
				return nil, fmt.Errorf("adopting observed failure domain for node %s: %w", node.ID, err)
			}
		case node.HasInfrastructureProvisioningStarted():
			// The scheduler now starts ProvisionNode only after a decision is
			// persisted. This branch handles nodes whose infrastructure already
			// existed or was underway when this feature was deployed.
			hasUnobserved = true
		default:
			unplaced = append(unplaced, node)
		}
	}

	// In order to ensure a balanced topology, we can't proceed
	// until unobserved, existing nodes get a placement decision. The allocator is
	// unavailable even when no existing node is waiting, because a node created
	// later in this pass would otherwise be placed from incomplete counts.
	if hasUnobserved {
		placement.allocator = nil
		if len(unplaced) > 0 {
			placement.markDeferred()
		}
		return placement, nil
	}

	// Nodes that were pending and unclaimed when this feature was deployed are
	// still safe to place before the scheduler claims them.
	for _, node := range unplaced {
		node.FailureDomain = new(allocator.take())
		if _, err := r.db.UpdateNode(ctx, node); err != nil {
			return nil, fmt.Errorf("persisting failure domain for node %s: %w", node.ID, err)
		}
	}
	return placement, nil
}

// recordNoFailureDomain backfills every undecided node with "", the placement
// for a provider that advertises no domains.
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
