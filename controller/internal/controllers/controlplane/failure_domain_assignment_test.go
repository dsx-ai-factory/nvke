// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestLeastUsedFailureDomainSelection(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		counts map[string]int
		want   string
	}{
		"single domain is always the selection": {
			counts: map[string]int{"fd-a": 7},
			want:   "fd-a",
		},
		"unused domain sorts before other unused domains": {
			counts: map[string]int{"fd-c": 0, "fd-a": 0, "fd-b": 0},
			want:   "fd-a",
		},
		"unused domain wins over used domains": {
			counts: map[string]int{"fd-a": 1, "fd-b": 1, "fd-c": 0},
			want:   "fd-c",
		},
		"least-used domain wins when every domain is used": {
			counts: map[string]int{"fd-a": 2, "fd-b": 1},
			want:   "fd-b",
		},
		"lexical order breaks equal-count ties": {
			counts: map[string]int{"fd-b": 1, "fd-a": 1},
			want:   "fd-a",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, leastUsedFailureDomainSelection(tc.counts))
		})
	}
}

func TestDomainAllocatorTake(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		eligible []string
		recorded []string
		want     string
	}{
		"no eligible domains leaves placement empty": {
			want: "",
		},
		"recording into an allocator with no domains changes nothing": {
			recorded: []string{"fd-a"},
			want:     "",
		},
		"unnamed eligible domains are dropped": {
			eligible: []string{""},
			want:     "",
		},
		"recorded placements steer the next one": {
			eligible: []string{"fd-a", "fd-b"},
			recorded: []string{"fd-a"},
			want:     "fd-b",
		},
		"recorded domains outside the eligible set are ignored": {
			eligible: []string{"fd-a", "fd-b"},
			recorded: []string{"retired-domain", "fd-a"},
			want:     "fd-b",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			allocator := newDomainAllocator(leastUsedFailureDomainSelection, tc.eligible)
			for _, domain := range tc.recorded {
				allocator.record(domain)
			}

			assert.Equal(t, tc.want, allocator.take())
		})
	}
}

// TestDomainAllocatorTakeCountsItsOwnHandouts catches a take() that selects without
// counting, which would hand every node the same domain. The stub selection
// makes the assertion independent of the least-used policy.
func TestDomainAllocatorTakeCountsItsOwnHandouts(t *testing.T) {
	t.Parallel()

	var seen []map[string]int
	stub := func(counts map[string]int) string {
		seen = append(seen, maps.Clone(counts))
		return "fd-a"
	}
	allocator := newDomainAllocator(stub, []string{"fd-a", "fd-b"})

	assert.Equal(t, "fd-a", allocator.take())
	assert.Equal(t, "fd-a", allocator.take())

	require.Len(t, seen, 2)
	assert.Equal(t, map[string]int{"fd-a": 0, "fd-b": 0}, seen[0])
	assert.Equal(t, map[string]int{"fd-a": 1, "fd-b": 0}, seen[1])
}

func TestDomainAllocatorSpreadsSuccessiveTakes(t *testing.T) {
	t.Parallel()

	allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-b", "fd-a", "fd-c"})

	got := []string{allocator.take(), allocator.take(), allocator.take(), allocator.take()}

	assert.Equal(t, []string{"fd-a", "fd-b", "fd-c", "fd-a"}, got)
}

func TestDomainAllocatorTakeWithoutEligibleDomainsIsRepeatable(t *testing.T) {
	t.Parallel()

	// A nil selection proves take() short-circuits before consulting it.
	allocator := newDomainAllocator(nil, nil)

	assert.Empty(t, allocator.take())
	assert.Empty(t, allocator.take())
}

// TestDomainAllocatorCollapsesDuplicateEligibleDomains pins that a domain listed
// twice is one placement slot, not two.
func TestDomainAllocatorCollapsesDuplicateEligibleDomains(t *testing.T) {
	t.Parallel()

	allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-a", "fd-a", "fd-b"})

	got := []string{allocator.take(), allocator.take(), allocator.take(), allocator.take()}

	assert.Equal(t, []string{"fd-a", "fd-b", "fd-a", "fd-b"}, got)
}

func TestAssignFailureDomainsBackfillsAndSpreadsUnplacedNodes(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-a",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Provisioned: db.NewCondition(true, "provisioned"),
			},
		},
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-b",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-c",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	for nodeName, domain := range map[string]string{
		"cp-a": "fd-a",
		"cp-b": "fd-b",
		"cp-c": "fd-c",
	} {
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
			return node.Name == nodeName && ptr.Deref(node.FailureDomain, "") == domain
		})).Return(db.Node{}, nil).Once()
	}

	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomainDiscoverySucceeded: true,
		failureDomains: failureDomains{
			eligible:           []string{"fd-c", "fd-b", "fd-a"},
			observedByNodeName: map[string]string{"cp-a": "fd-a"},
		},
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsDefersUntilExistingPlacementIsObserved(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-claimed",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending:             db.NewCondition(true, "waiting for infrastructure allocation"),
				ProvisioningClaimed: db.NewCondition(true, "provisioning workflow claimed node"),
			},
		},
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-pending",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomains:                  failureDomains{eligible: []string{"fd-a", "fd-b"}},
		failureDomainDiscoverySucceeded: true,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.True(t, requeue)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)

	// Once CAPI reports the claimed node's placement, the allocator is whole again
	// and the pending node takes the other domain.
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.Name == "cp-claimed" && ptr.Deref(node.FailureDomain, "") == "fd-a"
	})).Return(db.Node{}, nil).Once()
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.Name == "cp-pending" && ptr.Deref(node.FailureDomain, "") == "fd-b"
	})).Return(db.Node{}, nil).Once()
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	r.failureDomains.observedByNodeName = map[string]string{"cp-claimed": "fd-a"}

	requeue, err = r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsDefersWhenDiscoveryFailed(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		Name:         "cp-pending",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending: db.NewCondition(true, "pending"),
		},
	}}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomainDiscoverySucceeded: false,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.True(t, requeue)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsWithNothingToPlaceDoesNotRequeue(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		Name:         "cp-claimed",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending:             db.NewCondition(true, "waiting for infrastructure allocation"),
			ProvisioningClaimed: db.NewCondition(true, "provisioning workflow claimed node"),
		},
	}}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomainDiscoverySucceeded: false,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsMarksUnsupportedProviderComplete(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		Name:         "cp-pending",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending: db.NewCondition(true, "pending"),
		},
	}}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.Name == "cp-pending" && node.FailureDomain != nil && *node.FailureDomain == ""
	})).Return(db.Node{}, nil).Once()
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomains:                  failureDomains{},
		failureDomainDiscoverySucceeded: true,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsSkipsNodesMarkedForDeletion(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-deleting",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Deleting: db.NewCondition(true, "deleting"),
			},
		},
		{
			ID:            uuid.New(),
			ClusterID:     clusterID,
			Name:          "cp-placed",
			ControlPlane:  true,
			FailureDomain: new("fd-a"),
			Conditions: db.NodeConditions{
				Provisioned: db.NewCondition(true, "provisioned"),
			},
		},
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			Name:         "cp-pending",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.Name == "cp-pending" && ptr.Deref(node.FailureDomain, "") == "fd-b"
	})).Return(db.Node{}, nil).Once()
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomains:                  failureDomains{eligible: []string{"fd-a", "fd-b"}},
		failureDomainDiscoverySucceeded: true,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsSurfacesDatabaseErrors(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	claimed := db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		Name:         "cp-claimed",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending:             db.NewCondition(true, "waiting for infrastructure allocation"),
			ProvisioningClaimed: db.NewCondition(true, "provisioning workflow claimed node"),
		},
	}
	pending := db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		Name:         "cp-pending",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending: db.NewCondition(true, "pending"),
		},
	}

	tests := map[string]struct {
		nodes    []db.Node
		observed map[string]string
		listErr  error
		wantErr  string
	}{
		"listing control-plane nodes fails": {
			listErr: errors.New("connection refused"),
			wantErr: "failed to list control plane nodes",
		},
		"adopting an observed placement fails": {
			nodes:    []db.Node{claimed},
			observed: map[string]string{"cp-claimed": "fd-a"},
			wantErr:  "adopting observed failure domain for node",
		},
		"persisting a new placement fails": {
			nodes:   []db.Node{pending},
			wantErr: "persisting failure domain for node",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mockTx := &testutil.MockTx{}
			mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(tc.nodes, tc.listErr).Once()
			if tc.listErr == nil {
				mockTx.On("UpdateNode", mock.Anything, mock.Anything).
					Return(db.Node{}, errors.New("deadlock detected")).Once()
			}
			r := &reconciler{
				cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
				db:                              mockTx,
				failureDomainDiscoverySucceeded: true,
				failureDomains: failureDomains{
					eligible:           []string{"fd-a", "fd-b"},
					observedByNodeName: tc.observed,
				},
			}

			requeue, err := r.assignFailureDomains(context.Background(), clusterID)

			require.ErrorContains(t, err, tc.wantErr)
			assert.False(t, requeue)
			mockTx.AssertExpectations(t)
		})
	}
}

func TestAssignFailureDomainsSkippedWhenControllerDoesNotOwnTheControlPlane(t *testing.T) {
	t.Parallel()

	mockTx := &testutil.MockTx{}
	r := &reconciler{cfg: config.Config{}, db: mockTx}

	requeue, err := r.assignFailureDomains(context.Background(), uuid.New())

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
}

func TestAssignFailureDomainsReconcilesNodesOnAProviderWithoutDomains(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{
		{
			ID: uuid.New(), ClusterID: clusterID, Name: "cp-provisioned", ControlPlane: true,
			Conditions: db.NodeConditions{Provisioned: db.NewCondition(true, "provisioned")},
		},
		{
			ID: uuid.New(), ClusterID: clusterID, Name: "cp-pending", ControlPlane: true,
			Conditions: db.NodeConditions{Pending: db.NewCondition(true, "pending")},
		},
		{
			ID: uuid.New(), ClusterID: clusterID, Name: "cp-deleting", ControlPlane: true,
			Conditions: db.NodeConditions{Deleting: db.NewCondition(true, "deleting")},
		},
		{
			ID: uuid.New(), ClusterID: clusterID, Name: "cp-settled", ControlPlane: true,
			FailureDomain: new(""),
			Conditions:    db.NodeConditions{Provisioned: db.NewCondition(true, "provisioned")},
		},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	for _, name := range []string{"cp-provisioned", "cp-pending"} {
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
			return node.Name == name && node.FailureDomain != nil && *node.FailureDomain == ""
		})).Return(db.Node{}, nil).Once()
	}
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomains:                  failureDomains{},
		failureDomainDiscoverySucceeded: true,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.NoError(t, err)
	assert.False(t, requeue, "a provider without domains must not wait for an observation")
	mockTx.AssertExpectations(t)
}

func TestAssignFailureDomainsSurfacesErrorsRecordingAbsentDomains(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodes := []db.Node{{
		ID: uuid.New(), ClusterID: clusterID, Name: "cp-pending", ControlPlane: true,
		Conditions: db.NodeConditions{Pending: db.NewCondition(true, "pending")},
	}}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	mockTx.On("UpdateNode", mock.Anything, mock.Anything).
		Return(db.Node{}, errors.New("deadlock detected")).Once()
	r := &reconciler{
		cfg:                             config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                              mockTx,
		failureDomains:                  failureDomains{},
		failureDomainDiscoverySucceeded: true,
	}

	requeue, err := r.assignFailureDomains(context.Background(), clusterID)

	require.ErrorContains(t, err, "recording absent failure domain for node")
	assert.False(t, requeue)
	mockTx.AssertExpectations(t)
}
