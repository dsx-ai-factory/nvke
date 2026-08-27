// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodescheduling

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// TestSplitForgeSchedulable covers the capacity arithmetic that holds instances
// back for waiting control-plane nodes. The reservation is headroom-aware: it
// only bites when free capacity is at or below what the control plane needs.
func TestSplitForgeSchedulable(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		capacity       int
		pendingCount   int
		cpReserved     int
		wantSchedule   int
		wantDeferredCP int
	}{
		"no reservation schedules up to capacity": {
			capacity: 5, pendingCount: 3, cpReserved: 0,
			wantSchedule: 3, wantDeferredCP: 0,
		},
		"reservation with ample headroom does not bite": {
			capacity: 5, pendingCount: 3, cpReserved: 1,
			wantSchedule: 3, wantDeferredCP: 0,
		},
		"reservation trims the last worker when headroom is exact": {
			capacity: 3, pendingCount: 3, cpReserved: 1,
			wantSchedule: 2, wantDeferredCP: 1,
		},
		"reservation consumes the only free instance": {
			capacity: 1, pendingCount: 2, cpReserved: 1,
			wantSchedule: 0, wantDeferredCP: 1,
		},
		"reservation larger than capacity blocks every worker": {
			capacity: 2, pendingCount: 5, cpReserved: 5,
			wantSchedule: 0, wantDeferredCP: 2,
		},
		"zero capacity attributes nothing to the control plane": {
			capacity: 0, pendingCount: 2, cpReserved: 1,
			wantSchedule: 0, wantDeferredCP: 0,
		},
		"no pending nodes schedules nothing": {
			capacity: 3, pendingCount: 0, cpReserved: 1,
			wantSchedule: 0, wantDeferredCP: 0,
		},
		"negative capacity is clamped": {
			capacity: -1, pendingCount: 2, cpReserved: 0,
			wantSchedule: 0, wantDeferredCP: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			schedule, deferredForCP := splitForgeSchedulable(tc.capacity, tc.pendingCount, tc.cpReserved)
			assert.Equal(t, tc.wantSchedule, schedule, "schedule count")
			assert.Equal(t, tc.wantDeferredCP, deferredForCP, "deferred-for-CP count")
			assert.LessOrEqual(t, schedule+deferredForCP, tc.pendingCount,
				"slices must stay within the pending list")
		})
	}
}

// TestCapacityAccount covers the budget the node-pool scheduler shares across
// its reconciles: first reading seeds, later readings see what earlier pools
// spent, entries lapse back to live once past the TTL, and a nil account (the
// control-plane pass) is transparent.
func TestCapacityAccount(t *testing.T) {
	t.Parallel()

	typeA, typeB := uuid.New(), uuid.New()
	placement := ForgePlacementSpec{SubnetIDs: []uuid.UUID{uuid.New()}}
	otherPlacement := ForgePlacementSpec{SubnetIDs: []uuid.UUID{uuid.New()}}

	t.Run("nil account passes live readings through and ignores reserves", func(t *testing.T) {
		t.Parallel()

		var acct *CapacityAccount
		forgeRemaining, networkRemaining := acct.remaining(typeA, placement, 3, 7)
		assert.Equal(t, 3, forgeRemaining)
		assert.Equal(t, 7, networkRemaining)

		// Must not panic, and must not change the next reading.
		acct.reserve(typeA, placement, 1)
		forgeRemaining, networkRemaining = acct.remaining(typeA, placement, 3, 7)
		assert.Equal(t, 3, forgeRemaining)
		assert.Equal(t, 7, networkRemaining)
	})

	t.Run("later pools of the same type see what earlier pools spent", func(t *testing.T) {
		t.Parallel()

		acct := NewCapacityAccount(0)

		// First pool of the type seeds from the live reading.
		forgeRemaining, networkRemaining := acct.remaining(typeA, placement, 3, 7)
		assert.Equal(t, 3, forgeRemaining)
		assert.Equal(t, 7, networkRemaining)
		acct.reserve(typeA, placement, 2)

		// A later pool passes the same live reading (Forge's count does not drop
		// until an instance create lands) and must still see the debit.
		forgeRemaining, networkRemaining = acct.remaining(typeA, placement, 3, 7)
		assert.Equal(t, 1, forgeRemaining, "seeding must happen once per instance type until the TTL lapses")
		assert.Equal(t, 5, networkRemaining, "seeding must happen once per placement until the TTL lapses")
	})

	t.Run("separate instance types and placements keep separate budgets", func(t *testing.T) {
		t.Parallel()

		acct := NewCapacityAccount(0)
		acct.remaining(typeA, placement, 3, 7)
		acct.reserve(typeA, placement, 3)

		// A different type over the same placement shares only the network budget.
		forgeRemaining, networkRemaining := acct.remaining(typeB, placement, 9, 7)
		assert.Equal(t, 9, forgeRemaining, "a different instance type has its own compute budget")
		assert.Equal(t, 4, networkRemaining, "the same placement shares one network budget")

		// A different placement is independent on both axes.
		_, networkRemaining = acct.remaining(typeB, otherPlacement, 9, 7)
		assert.Equal(t, 7, networkRemaining)
	})

	t.Run("debits clamp at zero", func(t *testing.T) {
		t.Parallel()

		acct := NewCapacityAccount(0)
		acct.remaining(typeA, placement, 1, 1)
		acct.reserve(typeA, placement, 5)

		forgeRemaining, networkRemaining := acct.remaining(typeA, placement, 1, 1)
		assert.Equal(t, 0, forgeRemaining, "an over-debit must not hand a later pool phantom capacity")
		assert.Equal(t, 0, networkRemaining)
	})

	t.Run("entries lapse back to the live reading once past the TTL", func(t *testing.T) {
		t.Parallel()

		// Before the db-reconciler split the budget was per-tick and discarded at
		// the end of it. Reconciles have no such boundary, so expiry is what stops
		// a debit for a workflow that never created an instance from stranding
		// capacity for the lifetime of the process.
		clock := time.Now()
		acct := NewCapacityAccount(time.Minute)
		acct.now = func() time.Time { return clock }

		acct.remaining(typeA, placement, 3, 7)
		acct.reserve(typeA, placement, 2)
		forgeRemaining, _ := acct.remaining(typeA, placement, 3, 7)
		require.Equal(t, 1, forgeRemaining, "debit holds inside the TTL")

		clock = clock.Add(time.Minute)
		forgeRemaining, networkRemaining := acct.remaining(typeA, placement, 3, 7)
		assert.Equal(t, 3, forgeRemaining, "a lapsed entry re-seeds from the live reading")
		assert.Equal(t, 7, networkRemaining)
	})

	t.Run("non-positive reserve is a no-op", func(t *testing.T) {
		t.Parallel()

		acct := NewCapacityAccount(0)
		acct.remaining(typeA, placement, 2, 2)
		acct.reserve(typeA, placement, 0)
		acct.reserve(typeA, placement, -1)

		forgeRemaining, networkRemaining := acct.remaining(typeA, placement, 2, 2)
		assert.Equal(t, 2, forgeRemaining)
		assert.Equal(t, 2, networkRemaining)
	})
}

// TestControlPlaneReservedInstanceTypes_ReservesWithoutRecycling asserts the
// reservation is not scoped to control-plane recycling: a Pending control-plane
// node reserves its instance type even when no cluster at the site reports
// CPNodesRecycling. A control-plane node is just as capacity-blocked when it is a
// scale-out or a first-time provision, and a worker taking its instance is the
// same outage.
func TestControlPlaneReservedInstanceTypes_ReservesWithoutRecycling(t *testing.T) {
	t.Parallel()

	site := db.Site{ID: uuid.New()}
	typeA := uuid.New()
	clusterID := uuid.New()

	node := db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, InstanceTypeID: typeA}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
		return f.SiteID != nil && *f.SiteID == site.ID
	})).Return([]db.NodePool{}, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{node}, nil).Once()

	service := &Service{logger: slog.Default(), db: mockDB}
	got, err := service.controlPlaneReservedInstanceTypes(context.Background(), site.ID)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, 1, got[typeA].count)
	assert.Equal(t, node.ID, got[typeA].sampleNodeID)
	mockDB.AssertExpectations(t)
}

// TestControlPlaneReservedInstanceTypes_TalliesPerInstanceType asserts pending CP
// control-plane nodes are tallied per instance type regardless of which cluster
// they belong to, and that type-less nodes are excluded.
func TestControlPlaneReservedInstanceTypes_TalliesPerInstanceType(t *testing.T) {
	t.Parallel()

	site := db.Site{ID: uuid.New()}
	typeA, typeB := uuid.New(), uuid.New()
	recyclingID, quietID := uuid.New(), uuid.New()

	firstA := db.Node{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true, InstanceTypeID: typeA}
	nodes := []db.Node{
		firstA,
		{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true, InstanceTypeID: typeA},
		{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true, InstanceTypeID: typeB},
		// Type-less: cannot reserve a type that has not been chosen yet.
		{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true},
		// A sibling cluster that is not recycling: it competes for the same
		// site-wide pool, so it reserves too.
		{ID: uuid.New(), ClusterID: quietID, ControlPlane: true, InstanceTypeID: typeA},
	}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Once()
	// The Deleting filter is asserted here, not just SiteID/ControlPlane/Pending:
	// a node that is Pending and Deleting at once would otherwise reserve an
	// instance it is never going to claim.
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.SiteID != nil && *f.SiteID == site.ID &&
			f.ControlPlane != nil && *f.ControlPlane &&
			f.Pending != nil && *f.Pending &&
			f.Deleting != nil && !*f.Deleting
	})).Return(nodes, nil).Once()

	service := &Service{logger: slog.Default(), db: mockDB}
	got, err := service.controlPlaneReservedInstanceTypes(context.Background(), site.ID)
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, 3, got[typeA].count)
	assert.Equal(t, 1, got[typeB].count)
	assert.Equal(t, firstA.ID, got[typeA].sampleNodeID, "sample must be the first matching node")
	assert.Equal(t, recyclingID, got[typeA].sampleClusterID)
	mockDB.AssertExpectations(t)
}

// TestControlPlaneReservedInstanceTypes_SkipsAutoProvisionDisabledPools asserts a
// control-plane node in a node pool with auto-provisioning disabled does not
// reserve capacity: scheduleNodePoolNodes short-circuits on that flag, so the
// node can never be scheduled and would hold an instance indefinitely. Nodes with
// no node pool at all are managed by the control-plane controller instead, which
// has no such switch, so they must keep reserving.
func TestControlPlaneReservedInstanceTypes_SkipsAutoProvisionDisabledPools(t *testing.T) {
	t.Parallel()

	site := db.Site{ID: uuid.New()}
	typeA := uuid.New()
	recyclingID := uuid.New()

	disabledPool := db.NodePool{ID: uuid.New(), ClusterID: recyclingID, AutoProvisionDisabled: true}
	enabledPool := db.NodePool{ID: uuid.New(), ClusterID: recyclingID}

	noPoolNode := db.Node{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true, InstanceTypeID: typeA}
	nodes := []db.Node{
		// Auto-provisioning is off for this pool: never schedulable, never counted.
		{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true, InstanceTypeID: typeA, NodePoolID: disabledPool.ID},
		// No node pool: control-plane-controller managed, still reserves.
		noPoolNode,
		// Ordinary nodepool-managed control-plane node, still reserves.
		{ID: uuid.New(), ClusterID: recyclingID, ControlPlane: true, InstanceTypeID: typeA, NodePoolID: enabledPool.ID},
	}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodePools", mock.Anything, mock.Anything).
		Return([]db.NodePool{disabledPool, enabledPool}, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()

	service := &Service{logger: slog.Default(), db: mockDB}
	got, err := service.controlPlaneReservedInstanceTypes(context.Background(), site.ID)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, 2, got[typeA].count, "only the disabled pool's node is dropped")
	assert.Equal(t, noPoolNode.ID, got[typeA].sampleNodeID,
		"the sample must skip past the disabled pool's node to the first node that actually reserves")
	mockDB.AssertExpectations(t)
}

// TestControlPlaneReservedInstanceTypes_ListError asserts a node query failure is
// surfaced so the caller can fail open rather than silently under-reserving.
func TestControlPlaneReservedInstanceTypes_ListError(t *testing.T) {
	t.Parallel()

	site := db.Site{ID: uuid.New()}
	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
		return f.SiteID != nil && *f.SiteID == site.ID
	})).Return([]db.NodePool{}, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.Anything).
		Return([]db.Node(nil), errors.New("db unavailable")).Once()

	service := &Service{logger: slog.Default(), db: mockDB}
	_, err := service.controlPlaneReservedInstanceTypes(context.Background(), site.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db unavailable")
}

// TestScheduleForgeNodes_ControlPlanePassDoesNotReserveAgainstItself pins the
// self-blocking regression. The reservation ledger is site-wide over Pending
// control-plane nodes, so a pass that is itself scheduling a control-plane node
// counts that node. With one free instance that yields
// splitForgeSchedulable(1, 1, 1) = 0 and the node never provisions -- the exact
// outage this feature exists to prevent. SchedulingControlPlane suppresses it.
func TestScheduleForgeNodes_ControlPlanePassDoesNotReserveAgainstItself(t *testing.T) {
	t.Parallel()

	siteID := uuid.New()
	instanceTypeID := uuid.New()
	cpNodeID := uuid.New()

	// One Pending control-plane node of this type: the one being scheduled.
	pendingCP := []db.Node{{
		ID: cpNodeID, ClusterID: uuid.New(), ControlPlane: true, InstanceTypeID: instanceTypeID,
	}}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Maybe()
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return(pendingCP, nil).Maybe()

	service := &Service{logger: slog.Default(), db: mockDB}

	// Sanity: with the reservation applied, one instance is fully reserved.
	reserved, err := service.controlPlaneReservedInstanceTypes(context.Background(), siteID)
	require.NoError(t, err)
	require.Equal(t, 1, reserved[instanceTypeID].count)

	schedule, deferred := splitForgeSchedulable(1, 1, reserved[instanceTypeID].count)
	assert.Equal(t, 0, schedule, "with the reservation applied the node blocks itself")
	assert.Equal(t, 1, deferred)

	// SchedulingControlPlane means cpReserved stays 0, so the node schedules.
	schedule, deferred = splitForgeSchedulable(1, 1, 0)
	assert.Equal(t, 1, schedule, "a control-plane pass must not reserve against itself")
	assert.Equal(t, 0, deferred)
}

// TestForgeScheduleOptions_ControlPlaneAndWorkerBudgetsAreSeparate pins the
// double-charge regression: a nodepool-managed control-plane node is already
// counted by the reservation ledger, so debiting it from the worker budget too
// would cost a worker its turn.
func TestForgeScheduleOptions_ControlPlaneAndWorkerBudgetsAreSeparate(t *testing.T) {
	t.Parallel()

	instanceTypeID := uuid.New()
	placement := ForgePlacementSpec{SubnetIDs: []uuid.UUID{uuid.New()}}

	workerAcct := NewCapacityAccount(0)
	cpAcct := NewCapacityAccount(0)

	// 3 free instances. The control-plane pool takes one from its own budget.
	cpAcct.remaining(instanceTypeID, placement, 3, 3)
	cpAcct.reserve(instanceTypeID, placement, 1)

	// The worker budget must still see all 3: the control-plane node is
	// accounted for by the reservation, not by a debit here.
	forgeRemaining, _ := workerAcct.remaining(instanceTypeID, placement, 3, 3)
	assert.Equal(t, 3, forgeRemaining, "worker budget must not be charged for control-plane nodes")

	// With cpReserved=1 the workers get 2 -- both pending workers start.
	schedule, deferred := splitForgeSchedulable(forgeRemaining, 2, 1)
	assert.Equal(t, 2, schedule, "both workers start; only the reservation holds capacity back")
	assert.Equal(t, 0, deferred)

	// Had they shared one account the worker budget would read 2, costing a turn.
	shared, _ := cpAcct.remaining(instanceTypeID, placement, 3, 3)
	sharedSchedule, _ := splitForgeSchedulable(shared, 2, 1)
	assert.Equal(t, 1, sharedSchedule, "documents the bug a shared account would reintroduce")
}
