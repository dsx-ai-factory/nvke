// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
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
// domain balanced. Its zero value and an allocator with no eligible domains
// both hand out "", meaning no specific domain was requested.
type domainAllocator struct {
	machinesPerDomain map[string]int
	selection         failureDomainSelectionAlgorithm
}

// newDomainAllocator builds an allocator over the eligible domains. Unnamed
// domains are dropped because "" means no specific domain was requested.
func newDomainAllocator(selection failureDomainSelectionAlgorithm, eligible []string) domainAllocator {
	counts := make(map[string]int, len(eligible))
	for _, domain := range eligible {
		if domain != "" {
			counts[domain] = 0
		}
	}
	return domainAllocator{machinesPerDomain: counts, selection: selection}
}

// record counts a placement that already exists. Domains that are no longer
// eligible (i.e. not in cluster.status.failureDomains or flagged with controlPlane: false)
// are ignored, so they don't skew new placements.
func (a *domainAllocator) record(domain string) {
	if _, eligible := a.machinesPerDomain[domain]; eligible {
		a.machinesPerDomain[domain]++
	}
}

// take returns the next requested domain and counts it when applicable.
func (a *domainAllocator) take() string {
	if len(a.machinesPerDomain) == 0 {
		return ""
	}
	selected := a.selection(a.machinesPerDomain)
	a.machinesPerDomain[selected]++
	return selected
}

// newControlPlaneDomainAllocator counts active control-plane nodes once each.
// Prefer the Machine's observed or requested domain; a newly created node may
// not have a Machine yet, so its persisted decision is the fallback. Legacy
// nodes with neither are omitted until CAPI reports their placement.
func newControlPlaneDomainAllocator(nodes []db.Node, failureDomains *failureDomains) domainAllocator {
	allocator := newDomainAllocator(leastUsedFailureDomainSelection, failureDomains.eligible)
	for _, node := range nodes {
		if isMarkedForDeletion(node) {
			continue
		}
		domain := failureDomains.observedByNodeName[node.Name]
		if domain == "" {
			domain = node.FailureDomain
		}
		allocator.record(domain)
	}
	return allocator
}
