// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodescheduling

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultCapacityAccountTTL bounds how long a debit is believed. It has to
// outlive the window between starting a ProvisionNode workflow and Forge
// reporting the instance as used, without outliving it so far that a failed
// workflow strands capacity. Too long under-schedules; too short over-schedules.
const (
	DefaultCapacityAccountTTL = 2 * time.Minute
)

// CapacityAccount carries remaining Forge capacity across the reconciles of one
// scheduler, so node pools sharing an instance type draw from one budget instead
// of each seeing the whole of it.
//
// Without it the control-plane reservation is granted once per pool and delivers
// nothing on the multi-pool sites that are the normal shape of a Forge site:
// with three free instances of type T, one control-plane node waiting and three
// worker pools holding one pending node each, every pool independently computes
// min(max(0, 3-1), 1) == 1 and all three workers start, leaving the
// control-plane node nothing. It also bounds plain scale-out, which
// over-subscribes the same way without it: Forge's live unused-usable count does
// not drop until an instance create lands, so every pool in a pass sees capacity
// the earlier pools have already spent.
//
// Each scheduler keeps its own account and they must not be shared. A
// nodepool-managed control-plane node is still Pending while its provision
// workflow runs, so the reservation ledger counts it; were its own pool also
// debiting the worker account, workers would subtract it a second time. With 3
// free instances, 1 pending control-plane node and 2 pending workers that costs
// a worker its turn -- one starts where both should have.
//
// Before the db-reconciler split (#233) this was a per-tick value, created and
// discarded inside one Schedule call over a site's pools. There is no tick to
// scope it to now -- each pool is its own Reconcile -- so entries carry a
// seeding timestamp and are re-read live once older than ttl. That also makes it
// self-healing: a workflow that failed to start, or an instance that was freed,
// is picked up on the next expiry rather than being stranded for the process
// lifetime.
//
// Both maps are keyed the way controlPlaneCapacityAccount keys them: compute by
// instance type, network by canonical placement, since network capacity is a
// property of the subnets/prefixes and not of the instance type.
type CapacityAccount struct {
	mu               sync.Mutex
	ttl              time.Duration
	now              func() time.Time
	forgeRemaining   map[uuid.UUID]capacityEntry
	networkRemaining map[string]capacityEntry
}

type capacityEntry struct {
	remaining int
	seededAt  time.Time
}

// NewCapacityAccount creates an account whose entries expire after ttl. A
// non-positive ttl falls back to DefaultCapacityAccountTTL.
func NewCapacityAccount(ttl time.Duration) *CapacityAccount {
	if ttl <= 0 {
		ttl = DefaultCapacityAccountTTL
	}
	return &CapacityAccount{
		ttl:              ttl,
		now:              time.Now,
		forgeRemaining:   make(map[uuid.UUID]capacityEntry),
		networkRemaining: make(map[string]capacityEntry),
	}
}

// ForgeNetworkPlacementKey canonicalises a placement so two pools targeting the
// same subnets and prefixes share one network budget regardless of slice order.
func ForgeNetworkPlacementKey(placement ForgePlacementSpec) string {
	return fmt.Sprintf("%v|%v", sortedUUIDStrings(placement.SubnetIDs), sortedUUIDStrings(placement.VpcPrefixIDs))
}

func sortedUUIDStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	slices.Sort(out)
	return out
}

// remaining reports the compute and network capacity still unclaimed, seeding
// either counter from the caller's live reading the first time its key is seen
// or once the existing entry has aged past ttl. A nil account is the "no
// cross-pool accounting" case and hands the live readings straight back, which
// is what the control-plane pass wants.
func (acct *CapacityAccount) remaining(
	instanceTypeID uuid.UUID,
	placement ForgePlacementSpec,
	liveForge, liveNetwork int,
) (int, int) {
	if acct == nil {
		return liveForge, liveNetwork
	}

	acct.mu.Lock()
	defer acct.mu.Unlock()

	now := acct.now()
	placementKey := ForgeNetworkPlacementKey(placement)

	forge, seeded := acct.forgeRemaining[instanceTypeID]
	if !seeded || now.Sub(forge.seededAt) >= acct.ttl {
		forge = capacityEntry{remaining: max(0, liveForge), seededAt: now}
		acct.forgeRemaining[instanceTypeID] = forge
	}
	network, seeded := acct.networkRemaining[placementKey]
	if !seeded || now.Sub(network.seededAt) >= acct.ttl {
		network = capacityEntry{remaining: max(0, liveNetwork), seededAt: now}
		acct.networkRemaining[placementKey] = network
	}

	return forge.remaining, network.remaining
}

// reserve debits count instances and count IPs from the budget. Callers debit
// only after a ProvisionNode workflow has actually started, so a failed start
// leaves the capacity available to the next pool. A nil account is a no-op.
//
// The debit does not refresh seededAt: expiry is measured from the live reading
// the entry was seeded with, so a busy instance type still re-reads Forge every
// ttl instead of being held open by a steady trickle of debits.
func (acct *CapacityAccount) reserve(instanceTypeID uuid.UUID, placement ForgePlacementSpec, count int) {
	if acct == nil || count <= 0 {
		return
	}

	acct.mu.Lock()
	defer acct.mu.Unlock()

	// Seeding is the caller's job via remaining; an unseeded key here would mean
	// a reserve without a preceding capacity check, so clamp at zero rather than
	// letting the counter go negative and hand a later pool phantom capacity.
	placementKey := ForgeNetworkPlacementKey(placement)
	forge := acct.forgeRemaining[instanceTypeID]
	forge.remaining = max(0, forge.remaining-count)
	acct.forgeRemaining[instanceTypeID] = forge

	network := acct.networkRemaining[placementKey]
	network.remaining = max(0, network.remaining-count)
	acct.networkRemaining[placementKey] = network
}
