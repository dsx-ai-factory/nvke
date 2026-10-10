// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/clients"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestDeleteNode_UnhealthySkips(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodeSetID := uuid.New()
	nodePoolID := uuid.New()
	cluster := db.Cluster{
		ID:                 clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{MaxSurge: 1},
	}
	surgeNode := makeSurgeHealthyNode()
	surgeNode.ClusterID = clusterID
	poolNode := makePoolHealthyNode("pool-1", nodeSetID, nodePoolID)
	poolNode.ClusterID = clusterID
	pools := []db.NodePool{
		{ClusterID: clusterID, ControlPlane: true, DesiredCount: 2, MaxSurge: 0},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane
	})).Return([]db.Node{surgeNode, poolNode}, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID
	})).Return(pools, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{clusterControlPlaneConfig: cluster.ControlPlaneConfig}
	got, err := r.deleteNode(context.Background(), cluster, cp, surgeNode, "test", slog.Default())
	// An unhealthy control plane is a reason to wait, not a reconcile failure, so
	// this reports "did not start" rather than returning an error.
	require.NoError(t, err)
	assert.False(t, got)
	mockTx.AssertExpectations(t)
}

func TestScaleDownSurgeNodes_SkipsWhenNotClusterLevelSurge(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                 clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{MaxSurge: 1},
	}
	cpNodePools := []db.NodePool{
		{ClusterID: clusterID, ControlPlane: true, DesiredCount: 2, MaxSurge: 1},
	}

	r := &reconciler{logger: slog.Default()}
	cp := &controlPlane{clusterControlPlaneConfig: cluster.ControlPlaneConfig}
	handled, err := r.scaleDownSurgeNodes(context.Background(), cluster, cp, cpNodePools, slog.Default())
	require.NoError(t, err)
	assert.False(t, handled)
}

func TestScaleDownSurgeNodes_UnhealthySkips(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodeSetID := uuid.New()
	nodePoolID := uuid.New()
	cluster := db.Cluster{
		ID:                 clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{MaxSurge: 1},
	}
	surgeNode := makeSurgeHealthyNode()
	surgeNode.ClusterID = clusterID
	poolNode := makePoolHealthyNode("pool-1", nodeSetID, nodePoolID)
	poolNode.ClusterID = clusterID
	cpNodePools := []db.NodePool{
		{ClusterID: clusterID, ControlPlane: true, DesiredCount: 2, MaxSurge: 0},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane
	})).Return([]db.Node{surgeNode, poolNode}, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID
	})).Return(cpNodePools, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{clusterControlPlaneConfig: cluster.ControlPlaneConfig}
	handled, err := r.scaleDownSurgeNodes(context.Background(), cluster, cp, cpNodePools, slog.Default())
	require.NoError(t, err)
	assert.False(t, handled)
	mockTx.AssertExpectations(t)
}

func TestCreatePendingNode_RequiresControlPlaneConfigurationID(t *testing.T) {
	t.Parallel()

	mockTx := &testutil.MockTx{}
	r := &reconciler{db: mockTx, logger: slog.Default()}

	cluster := db.Cluster{
		ID: uuid.New(),
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: uuid.Nil,
		},
	}

	_, err := r.createPendingNode(context.Background(), cluster, nil, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing control plane configuration ID")
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestCreatePendingNodeSetsFailureDomainButLeavesInstanceTypeUnset pins that
// placement is part of node creation while the scheduler remains the only
// writer of the instance type.
func TestCreatePendingNodeSetsFailureDomainButLeavesInstanceTypeUnset(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	controlPlaneConfigID := uuid.New()
	failureDomain := "fd-a"

	mockTx := &testutil.MockTx{}
	mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ClusterID == clusterID &&
			node.ControlPlane &&
			node.NodePoolID == uuid.Nil &&
			node.NodeSetID == uuid.Nil &&
			node.NicoInstanceTypeID() == uuid.Nil &&
			node.ControlPlaneConfigurationID == controlPlaneConfigID &&
			node.FailureDomain == failureDomain
	})).Return(db.Node{}, nil).Once()

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "test-cluster",
		ProviderType: db.ProviderTypeCAPI,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: controlPlaneConfigID,
		},
	}

	newNodeProvider, err := clients.GetNewNodeProvider(cluster)
	require.NoError(t, err)

	_, err = r.createPendingNode(context.Background(), cluster, nil, failureDomain, newNodeProvider)
	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

func TestCreatePendingNode_ProviderPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		providerType    db.ProviderType
		wantNicoPayload bool
	}{
		{
			name:            "nico cluster nodes carry a payload for strict readers",
			providerType:    db.ProviderTypeNico,
			wantNicoPayload: true,
		},
		{
			name:            "capi cluster nodes carry no nico payload",
			providerType:    db.ProviderTypeCAPI,
			wantNicoPayload: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			controlPlaneConfigID := uuid.New()
			mockTx := &testutil.MockTx{}
			mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
				return (node.Nico != nil) == tt.wantNicoPayload &&
					node.NicoInstanceTypeID() == uuid.Nil
			})).Return(db.Node{}, nil).Once()

			r := &reconciler{db: mockTx, logger: slog.Default()}
			cluster := db.Cluster{
				ID:           uuid.New(),
				Name:         "test-cluster",
				ProviderType: tt.providerType,
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID: controlPlaneConfigID,
				},
			}

			newNodeProvider, err := clients.GetNewNodeProvider(cluster)
			require.NoError(t, err)

			_, err = r.createPendingNode(context.Background(), cluster, nil, "", newNodeProvider)
			require.NoError(t, err)
			mockTx.AssertExpectations(t)
		})
	}
}

func TestReconcile_SkipsClusterBeingDeleted(t *testing.T) {
	t.Parallel()

	mockTx := &testutil.MockTx{}
	r := &reconciler{
		cfg:    config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:     mockTx,
		logger: slog.Default(),
	}

	cluster := db.Cluster{
		ID:                 uuid.New(),
		ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3, ConfigurationID: uuid.New()},
	}
	cluster.Conditions.Deleting = db.NewCondition(true, "cluster teardown in progress")

	err := r.reconcile(context.Background(), cluster)
	require.NoError(t, err)
	// The teardown workflow owns CP node deletion; the reconciler must not read or
	// (re)create nodes for a cluster being torn down.
	mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

func TestReconcileControlPlaneScaleUp_FeatureFlagDisabled(t *testing.T) {
	t.Parallel()

	mockTx := &testutil.MockTx{}
	r := &reconciler{
		cfg:    config.Config{ProvisionControlPlaneWithControlPlaneController: false},
		db:     mockTx,
		logger: slog.Default(),
	}

	cluster := db.Cluster{
		ID: uuid.New(),
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: uuid.New(),
			DesiredCount:    3,
		},
	}
	cp := &controlPlane{}

	err := r.reconcileControlPlaneScaleUp(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	mockTx.AssertNotCalled(t, "ListClusterNodeNames", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

func TestReconcileControlPlaneScaleUp_FeatureFlagEnabled(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	controlPlaneConfigID := uuid.New()

	mockTx := &testutil.MockTx{}
	mockTx.On("ListClusterNodeNames", mock.Anything, clusterID).Return([]string{"existing-node"}, nil).Once()
	mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ClusterID == clusterID &&
			node.NodePoolID == uuid.Nil &&
			node.NodeSetID == uuid.Nil &&
			node.ControlPlane &&
			// The control-plane controller no longer stamps an instance type;
			// the control plane node scheduler selects it at schedule time.
			node.NicoInstanceTypeID() == uuid.Nil &&
			node.ControlPlaneConfigurationID == controlPlaneConfigID &&
			node.FailureDomain != ""
	})).Return(db.Node{}, nil).Twice()

	r := &reconciler{
		cfg: config.Config{
			ProvisionControlPlaneWithControlPlaneController: true,
		},
		db:     mockTx,
		logger: slog.Default(),
	}

	cluster := db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeNico,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: controlPlaneConfigID,
			DesiredCount:    3,
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.New(),
		},
	}
	// One existing no-nodepool control-plane node => deficit is 2.
	cp := &controlPlane{
		nodes: []db.Node{
			{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, NodePoolID: uuid.Nil},
		},
	}

	allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-a", "fd-b"})
	newNodeProvider, err := clients.GetNewNodeProvider(cluster)
	require.NoError(t, err)
	err = r.reconcileControlPlaneScaleUp(context.Background(), cluster, cp, allocator, newNodeProvider)
	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

func TestScaleUpSharesFailureDomainAllocatorAcrossCreationSites(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{
		ID:           uuid.New(),
		ProviderType: db.ProviderTypeCAPI,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: uuid.New(),
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("ListClusterNodeNames", mock.Anything, cluster.ID).Return([]string{}, nil).Twice()
	var placed []string
	mockTx.On("CreateNode", mock.Anything, mock.AnythingOfType("*db.Node")).Run(func(args mock.Arguments) {
		node := args.Get(1).(*db.Node)
		placed = append(placed, node.FailureDomain)
	}).Return(db.Node{}, nil).Times(3)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-a", "fd-b"})
	newNodeProvider, err := clients.GetNewNodeProvider(cluster)
	require.NoError(t, err)

	created, err := r.scaleUp(context.Background(), cluster, 2, "control-plane scale-up", allocator, newNodeProvider)
	require.NoError(t, err)
	assert.Equal(t, 2, created)
	created, err = r.scaleUp(context.Background(), cluster, 1, "surge node during upgrade", allocator, newNodeProvider)
	require.NoError(t, err)
	assert.Equal(t, 1, created)

	assert.Equal(t, []string{"fd-a", "fd-b", "fd-a"}, placed)
	mockTx.AssertExpectations(t)
}

func TestScaleUpUsesAllocatorDecision(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		allocator domainAllocator
		want      string
	}{
		"legacy node-pool mode requests no specific domain": {
			want: "",
		},
		"allocator without eligible domains requests no specific domain": {
			allocator: newDomainAllocator(leastUsedFailureDomainSelection, nil),
			want:      "",
		},
		"eligible domain is assigned": {
			allocator: newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-a"}),
			want:      "fd-a",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cluster := db.Cluster{
				ID:           uuid.New(),
				ProviderType: db.ProviderTypeCAPI,
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID: uuid.New(),
				},
			}
			mockTx := &testutil.MockTx{}
			mockTx.On("ListClusterNodeNames", mock.Anything, cluster.ID).Return([]string{}, nil).Once()
			var placement string
			mockTx.On("CreateNode", mock.Anything, mock.AnythingOfType("*db.Node")).Run(func(args mock.Arguments) {
				placement = args.Get(1).(*db.Node).FailureDomain
			}).Return(db.Node{}, nil).Once()

			r := &reconciler{db: mockTx, logger: slog.Default()}
			newNodeProvider, err := clients.GetNewNodeProvider(cluster)
			require.NoError(t, err)
			created, err := r.scaleUp(context.Background(), cluster, 1, "control-plane scale-up", tc.allocator, newNodeProvider)
			require.NoError(t, err)
			assert.Equal(t, 1, created)
			assert.Equal(t, tc.want, placement)
			mockTx.AssertExpectations(t)
		})
	}
}

func TestReconcileControlPlaneScaleUp_Deficit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		desiredCount int
		existing     int
		wantCreated  int
	}{
		{name: "creates deficit nodes", desiredCount: 3, existing: 1, wantCreated: 2},
		{name: "creates all when none exist", desiredCount: 2, existing: 0, wantCreated: 2},
		{name: "no-op when at desired", desiredCount: 2, existing: 2, wantCreated: 0},
		{name: "no-op when above desired", desiredCount: 1, existing: 3, wantCreated: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clusterID := uuid.New()
			controlPlaneConfigID := uuid.New()

			mockTx := &testutil.MockTx{}
			if tc.wantCreated > 0 {
				mockTx.On("ListClusterNodeNames", mock.Anything, clusterID).Return([]string{}, nil).Once()
				mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
					return node.ControlPlane &&
						node.NodePoolID == uuid.Nil &&
						node.NodeSetID == uuid.Nil &&
						node.NicoInstanceTypeID() == uuid.Nil &&
						node.ControlPlaneConfigurationID == controlPlaneConfigID
				})).Return(db.Node{}, nil).Times(tc.wantCreated)
			}

			r := &reconciler{
				cfg:    config.Config{ProvisionControlPlaneWithControlPlaneController: true},
				db:     mockTx,
				logger: slog.Default(),
			}

			cluster := db.Cluster{
				ID:           clusterID,
				ProviderType: db.ProviderTypeNico,
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID: controlPlaneConfigID,
					DesiredCount:    tc.desiredCount,
				},
				Nico: &db.NicoClusterProvider{
					SiteID: uuid.New(),
				},
			}

			nodes := make([]db.Node, 0, tc.existing)
			for range tc.existing {
				nodes = append(nodes, db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, NodePoolID: uuid.Nil})
			}
			cp := &controlPlane{nodes: nodes}

			allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-a", "fd-b"})
			newNodeProvider, err := clients.GetNewNodeProvider(cluster)
			require.NoError(t, err)
			err = r.reconcileControlPlaneScaleUp(context.Background(), cluster, cp, allocator, newNodeProvider)
			require.NoError(t, err)
			mockTx.AssertExpectations(t)
			if tc.wantCreated == 0 {
				mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
			}
		})
	}
}
