// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package failuredomains

import (
	"math"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

// SelectionAlgorithm picks the next failure domain from the
// current placement count per eligible domain.
//
// It is called only with a non-empty map, must be deterministic, and must not
// modify the map. Counting the returned domain belongs to the caller.
type SelectionAlgorithm func(counts map[string]int) string

// LeastUsed returns the domain with the fewest
// placements, breaking ties by domain name.
func LeastUsed(counts map[string]int) string {
	selected, fewest := "", math.MaxInt
	for domain, count := range counts {
		if count < fewest || (count == fewest && domain < selected) {
			selected, fewest = domain, count
		}
	}
	return selected
}

// Allocator hands out control-plane failure domains, keeping the count per
// domain balanced. Its zero value and an allocator with no eligible domains
// both hand out "", meaning no specific domain was requested.
type Allocator struct {
	machinesPerDomain map[string]int
	selection         SelectionAlgorithm
}

// NewAllocator builds an allocator over the eligible domains. Unnamed
// domains are dropped because "" means no specific domain was requested.
func NewAllocator(selection SelectionAlgorithm, eligible []string) Allocator {
	counts := make(map[string]int, len(eligible))
	for _, domain := range eligible {
		if domain != "" {
			counts[domain] = 0
		}
	}
	return Allocator{machinesPerDomain: counts, selection: selection}
}

// Record counts a placement that already exists. Domains that are no longer
// eligible (i.e. not in cluster.status.failureDomains or flagged with controlPlane: false)
// are ignored, so they don't skew new placements.
func (a *Allocator) Record(domain string) {
	if _, eligible := a.machinesPerDomain[domain]; eligible {
		a.machinesPerDomain[domain]++
	}
}

// Release uncounts a node that a surge will replace, so the surge is placed as if
// that node were already gone. Ineligible or uncounted domains are ignored.
func (a *Allocator) Release(domain string) {
	if a.machinesPerDomain[domain] > 0 {
		a.machinesPerDomain[domain]--
	}
}

// Take returns the next requested domain and counts it when applicable.
func (a *Allocator) Take() string {
	if len(a.machinesPerDomain) == 0 {
		return ""
	}
	selected := a.selection(a.machinesPerDomain)
	a.machinesPerDomain[selected]++
	return selected
}

// Exclude removes domains from the allocator, so Take never returns them.
func (a *Allocator) Exclude(domains ...string) {
	for _, domain := range domains {
		delete(a.machinesPerDomain, domain)
	}
}

// NewControlPlaneAllocator counts active control-plane nodes once each,
// skipping nodes with a delete recorded or in flight.
// Prefer the Machine's observed or requested domain; a newly created node may
// not have a Machine yet, so its persisted decision is the fallback. Legacy
// nodes with neither are omitted until CAPI reports their placement.
func NewControlPlaneAllocator(cpNodes []db.Node, snapshot *Snapshot) Allocator {
	allocator := NewAllocator(LeastUsed, snapshot.Eligible)
	for _, node := range cpNodes {
		if nodes.IsDeleteInFlight(node) {
			continue
		}
		allocator.Record(snapshot.DomainOf(node))
	}
	return allocator
}

// NodesWithoutKnownDomain returns the names of active nodes whose placement is
// unknown while the cluster has eligible domains. The allocator leaves them
// uncounted, so new nodes may land in the domain they occupy.
func NodesWithoutKnownDomain(cpNodes []db.Node, snapshot *Snapshot) []string {
	if len(snapshot.Eligible) == 0 {
		return nil
	}
	var names []string
	for _, node := range cpNodes {
		if !nodes.IsDeleteInFlight(node) && snapshot.DomainOf(node) == "" {
			names = append(names, node.Name)
		}
	}
	return names
}
