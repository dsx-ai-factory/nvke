// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file holds the control-plane capacity-priority logic: a Pending
// control-plane node gets first claim on scarce Forge capacity of its instance
// type, ahead of worker nodes that would otherwise consume the instance it is
// waiting for.
//
// This is deliberately not scoped to control-plane recycling. A control-plane
// node is just as capacity-blocked when it is a fresh cluster's first node, a
// scale-out, or a replacement outside a recycle, and in each of those cases a
// worker taking the instance first is the same outage.
//
// It lives in nodescheduling because ScheduleForgeNodes is the single seam both
// schedulers pass through. Before the db-reconciler split (#233) the guarantee
// rested on ordering -- one per-site tick ran the control plane before workers.
// ControlPlaneNodeScheduler and NodePoolNodeScheduler are now independent
// controllers with no ordering between them, so the reservation below is the
// only thing enforcing the priority.
//
// Known limitation: this reserves capacity only against workers that have not yet
// started provisioning. A worker already inside a Forge instance create is not
// preempted, and nothing is reserved across the scale-in gap, so an instance
// freed before the control-plane node exists is unprotected. Closing that gap
// needs a reservation recorded before worker teardown, or an exact handoff of the
// scaled-in worker's MachineID.

package nodescheduling

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// splitForgeSchedulable divides a pass's pending nodes into the ones that may
// start now, the ones held back so waiting control-plane nodes can claim
// capacity first, and (implicitly) the ones with no capacity at all.
//
// capacity is the live Forge instance + network budget for the instance type;
// cpReserved is the number of Pending control-plane nodes competing for it.
//
// The reservation is headroom-aware: it only reduces the worker count when
// capacity is at or below cpReserved. With capacity 50 and one control-plane
// node waiting, 49 workers still start. This keeps the guarantee under
// contention without stalling all scale-out for an instance type whose
// control-plane node is stuck for an unrelated reason (e.g. subnet IP
// exhaustion).
//
// Callers slice their pending list as:
//
//	nodes[:schedule]                              -> provision now
//	nodes[schedule : schedule+deferredForCP]      -> deferred for control-plane priority
//	nodes[schedule+deferredForCP:]                -> deferred for lack of capacity
//
// The first result is the schedulable count, the second the count held back for
// the control plane. Named returns are not used: golangci-lint's nonamedreturns
// is enabled repo-wide and only excluded under internal/temporal/workflows/.
func splitForgeSchedulable(capacity, pendingCount, cpReserved int) (int, int) {
	capacity = max(0, capacity)
	pendingCount = max(0, pendingCount)
	cpReserved = max(0, cpReserved)

	// What this pass would schedule with no control-plane reservation.
	uncapped := min(capacity, pendingCount)
	schedule := min(max(0, capacity-cpReserved), pendingCount)

	return schedule, uncapped - schedule
}

// cpReservation records how many Pending control-plane nodes are competing for
// one instance type at a site, plus one representative node/cluster so the
// deferral log line names something an operator can look up.
//
// The sample IDs must reach the operator-facing log line only, never the
// deferred worker's node condition. A node's Pending condition, Message
// included, is served to tenants over the public API (apiNodeFromDB in
// internal/server/nodes.go), and the reservation is deliberately site-wide
// across clusters, so the blocking control-plane node frequently belongs to a
// different tenant's cluster. Naming its node/cluster UUIDs in a tenant-visible
// message would be cross-tenant information disclosure. That is why
// ScheduleForgeNodes uses only the count for its cap and the condition message
// stays generic.
type cpReservation struct {
	count           int
	sampleNodeID    uuid.UUID
	sampleClusterID uuid.UUID
}

// controlPlaneReservedInstanceTypes builds the per-instance-type reservation for
// one scheduling pass: every Pending control-plane node at the site that has a
// chosen instance type reserves one instance of that type.
//
// Scope is deliberately site-wide across clusters. Forge instance-type capacity
// is a shared site pool, so a worker in a sibling cluster can consume the very
// instance another cluster's control-plane node is waiting for.
//
// Pending is an accurate proxy for "has not claimed an instance yet": a Forge
// node only leaves Pending once its instance create succeeds (see
// provisionNodeInfrastructure in internal/temporal/workflows/provision_node.go).
//
// Nodes are not filtered through IsManagedByControlPlaneController: both
// nodepool-managed and no-nodepool control-plane pendings must reserve capacity.
func (s *Service) controlPlaneReservedInstanceTypes(
	ctx context.Context,
	siteID uuid.UUID,
) (map[uuid.UUID]cpReservation, error) {
	// The pools are read here rather than passed in: ScheduleForgeNodes is
	// reached from a per-cluster and a per-node-pool reconcile, neither of which
	// holds the site's full pool list the way the old per-site tick did.
	nodePools, err := s.db.ListNodePools(ctx, db.NodePoolFilters{SiteID: &siteID})
	if err != nil {
		return nil, fmt.Errorf("failed to list node pools: %w", err)
	}

	autoProvisionDisabled := make(map[uuid.UUID]struct{})
	for _, nodePool := range nodePools {
		if nodePool.AutoProvisionDisabled {
			autoProvisionDisabled[nodePool.ID] = struct{}{}
		}
	}

	// One site-scoped query covers every cluster; NodeFilters.SiteID joins
	// through the cluster's site.
	//
	// Deleting excludes nodes that are Pending and Deleting at once: they are on
	// their way out and will never claim the instance they would otherwise hold.
	// The filter matches asymmetrically, so false means "not definitely
	// deleting" and keeps nodes with no Deleting condition at all.
	pendingControlPlane, err := s.db.ListNodes(ctx, db.NodeFilters{
		SiteID:       &siteID,
		ControlPlane: ptr.To(true),
		Pending:      ptr.To(true),
		Deleting:     ptr.To(false),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pending control plane nodes: %w", err)
	}

	reserved := make(map[uuid.UUID]cpReservation)
	for _, node := range pendingControlPlane {
		// A type-less node has not picked a type yet, so there is nothing to
		// reserve. It gets a type stamped during control-plane scheduling and
		// will be counted on the next pass.
		if node.InstanceTypeID == uuid.Nil {
			continue
		}
		// reconcileNodePool short-circuits on AutoProvisionDisabled, so a
		// control-plane node in such a pool can never be scheduled and must not
		// hold capacity indefinitely. Only nodepool-managed nodes can be
		// disabled this way: a no-nodepool control-plane node has no pool to
		// check and still reserves.
		if node.IsManagedByNodePool() {
			if _, disabled := autoProvisionDisabled[node.NodePoolID]; disabled {
				continue
			}
		}

		res := reserved[node.InstanceTypeID]
		if res.count == 0 {
			res.sampleNodeID = node.ID
			res.sampleClusterID = node.ClusterID
		}
		res.count++
		reserved[node.InstanceTypeID] = res
	}

	return reserved, nil
}
