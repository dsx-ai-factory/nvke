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

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestIsSurgeAtClusterLevel(t *testing.T) {
	t.Parallel()

	clusterWithSurge := db.Cluster{
		ControlPlaneConfig: db.ControlPlaneConfig{MaxSurge: 1},
	}

	tests := []struct {
		name    string
		cluster db.Cluster
		pools   []db.NodePool
		want    bool
	}{
		{
			name:    "no cluster maxSurge",
			cluster: db.Cluster{},
			pools: []db.NodePool{
				{ControlPlane: true, MaxSurge: 1},
			},
			want: false,
		},
		{
			name:    "nodepool maxSurge handles surge",
			cluster: clusterWithSurge,
			pools: []db.NodePool{
				{ControlPlane: true, MaxSurge: 1},
			},
			want: false,
		},
		{
			name:    "cluster maxSurge without nodepool maxSurge",
			cluster: clusterWithSurge,
			pools: []db.NodePool{
				{ControlPlane: true, MaxSurge: 0},
			},
			want: true,
		},
		{
			name:    "cluster maxSurge ignored when any cp pool has maxSurge",
			cluster: clusterWithSurge,
			pools: []db.NodePool{
				{ControlPlane: true, MaxSurge: 0},
				{ControlPlane: true, MaxSurge: 1},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isSurgeAtClusterLevel(tt.cluster, tt.pools))
		})
	}
}

func TestCPNodePoolMaxSurgeEnabled(t *testing.T) {
	t.Parallel()

	assert.False(t, cpNodePoolMaxSurgeEnabled([]db.NodePool{
		{ControlPlane: true, MaxSurge: 0},
	}))
	assert.True(t, cpNodePoolMaxSurgeEnabled([]db.NodePool{
		{ControlPlane: true, MaxSurge: 1},
	}))
	assert.False(t, cpNodePoolMaxSurgeEnabled(nil))
}

func TestReconcileNodesNeedingRolloutWithoutControlPlaneVIPDoesNotReplaceNodes(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	oldConfigID := uuid.New()
	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "new-os"})
	pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	pinnedSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "old-os"})
	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "single-no-vip",
		EnableCPNodeRecycling: true,
		KubernetesVersion:     "1.31.0",
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: desiredSpec,
			DesiredCount:           1,
			MaxSurge:               1,
		},
	}
	node := db.Node{
		ID:                          uuid.New(),
		ClusterID:                   clusterID,
		ControlPlane:                true,
		ControlPlaneConfigurationID: oldConfigID,
		Name:                        "cp-0",
	}
	cp := &controlPlane{
		nodes:                     []db.Node{node},
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			oldConfigID: pinnedSpec,
		},
		useController: true,
	}
	mockTx := &testutil.MockTx{}
	cpTrue := true
	mockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	}).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	}).Return([]db.Node{node}, nil)
	r := &reconciler{cfg: cpControllerConfig(true), db: mockTx, logger: slog.Default()}

	err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{})

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestReconcileNodesNeedingRolloutWithoutControlPlaneVIPDeletesAfterSurgeExists(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	oldConfigID := uuid.New()
	currentConfigID := uuid.New()
	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "new-os"})
	pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	pinnedSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "old-os"})
	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "single-no-vip",
		EnableCPNodeRecycling: true,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: desiredSpec,
			DesiredCount:           1,
			MaxSurge:               1,
		},
		Conditions: db.ClusterConditions{
			CPNodesRecycling: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	oldNode := makeHealthyNode("cp-old", uuid.Nil)
	oldNode.ID = uuid.New()
	oldNode.ClusterID = clusterID
	oldNode.ControlPlane = true
	oldNode.ControlPlaneConfigurationID = oldConfigID
	newNode := makeHealthyNode("cp-new", uuid.Nil)
	newNode.ID = uuid.New()
	newNode.ClusterID = clusterID
	newNode.ControlPlane = true
	newNode.ControlPlaneConfigurationID = currentConfigID
	cp := &controlPlane{
		nodes:                     []db.Node{oldNode, newNode},
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			oldConfigID:     pinnedSpec,
			currentConfigID: desiredSpec,
		},
		useController: true,
	}
	mockTx := &testutil.MockTx{}
	cpTrue := true
	mockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	}).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	}).Return([]db.Node{oldNode, newNode}, nil).Twice()
	mockTx.On("GetNodeForUpdate", mock.Anything, oldNode.ID).Return(oldNode, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ID == oldNode.ID && node.Conditions.Deleting.Status == db.ConditionStatusTrue
	})).Return(oldNode, nil)
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil)
	r := &reconciler{cfg: cpControllerConfig(true), db: mockTx, logger: slog.Default()}

	err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{})

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

func TestReconcileLegacyNodesNeedingRolloutWithoutControlPlaneVIPDoesNotReplaceNodes(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	poolID := uuid.New()
	currentNodeSetID := uuid.New()
	deprecatedNodeSetID := uuid.New()
	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "legacy-no-vip",
		EnableCPNodeRecycling: true,
	}
	pool := db.NodePool{
		ID:               poolID,
		ClusterID:        clusterID,
		ControlPlane:     true,
		DesiredCount:     1,
		CurrentNodeSetID: &currentNodeSetID,
	}
	deprecatedNode := db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		NodePoolID:   poolID,
		NodeSetID:    deprecatedNodeSetID,
		ControlPlane: true,
		Name:         "cp-0",
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodePools", mock.Anything, mock.MatchedBy(func(filters db.NodePoolFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return([]db.NodePool{pool}, nil).Times(3)
	mockTx.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &poolID}).Return([]db.NodeSet{
		{ID: currentNodeSetID, NodePoolID: poolID},
		{ID: deprecatedNodeSetID, NodePoolID: poolID},
	}, nil)
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{NodeSetID: &deprecatedNodeSetID}).Return([]db.Node{deprecatedNode}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return([]db.Node{deprecatedNode}, nil)
	r := &reconciler{db: mockTx, logger: slog.Default()}

	err := r.reconcileNodesNeedingRollout(context.Background(), cluster, &controlPlane{}, domainAllocator{})

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestIsSurgeAtClusterLevel_InstanceTypePreferenceNotRequired(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{
		ControlPlaneConfig: db.ControlPlaneConfig{
			MaxSurge: 1,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				Nico: &db.NicoControlPlaneProvider{
					InstanceTypePreference: []uuid.UUID{uuid.New()},
				},
			},
		},
	}
	pools := []db.NodePool{{ControlPlane: true, MaxSurge: 0}}

	assert.True(t, isSurgeAtClusterLevel(cluster, pools))
}

func TestGetTotalDesiredControlPlaneNodes_WithSurge(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                 clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{MaxSurge: 1},
	}
	pools := []db.NodePool{
		{ClusterID: clusterID, ControlPlane: true, DesiredCount: 3, MaxSurge: 0},
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane
	})).Return(pools, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	ctx := context.Background()
	cp := &controlPlane{clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	withoutSurge, err := r.getTotalDesiredControlPlaneNodes(ctx, cluster, cp, false)
	require.NoError(t, err)
	assert.Equal(t, 3, withoutSurge)

	withSurge, err := r.getTotalDesiredControlPlaneNodes(ctx, cluster, cp, true)
	require.NoError(t, err)
	assert.Equal(t, 4, withSurge)

	mockTx.AssertExpectations(t)
}

func TestIsControlPlaneHealthy_WithSurgeThreshold(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodeSetID := uuid.New()
	cluster := db.Cluster{
		ID:                 clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{MaxSurge: 1},
	}
	poolsThree := []db.NodePool{
		{ClusterID: clusterID, ControlPlane: true, DesiredCount: 3, MaxSurge: 0},
	}
	//nolint:prealloc // exact-capacity literal is deliberate: the slice is also appended to below, and spare capacity would alias the shared backing array.
	healthyNodesThree := []db.Node{
		makeHealthyNode("cp-1", nodeSetID),
		makeHealthyNode("cp-2", nodeSetID),
		makeHealthyNode("cp-3", nodeSetID),
	}

	nodePoolID := uuid.New()
	poolsTwo := []db.NodePool{
		{ClusterID: clusterID, ControlPlane: true, DesiredCount: 2, MaxSurge: 0},
	}

	tests := []struct {
		name  string
		pools []db.NodePool
		nodes []db.Node
		want  bool
	}{
		{
			name:  "with surge three nodes insufficient",
			pools: poolsThree,
			nodes: healthyNodesThree,
			want:  false,
		},
		{
			name:  "with surge four nodes sufficient",
			pools: poolsThree,
			nodes: append(healthyNodesThree, makeHealthyNode("surge-1", nodeSetID)),
			want:  true,
		},
		{
			name:  "surge plus one pool node insufficient for scale down",
			pools: poolsTwo,
			nodes: []db.Node{
				makeSurgeHealthyNode(),
				makePoolHealthyNode("pool-1", nodeSetID, nodePoolID),
			},
			want: false,
		},
		{
			name:  "two pool plus surge healthy sufficient for scale down",
			pools: poolsTwo,
			nodes: []db.Node{
				makeSurgeHealthyNode(),
				makePoolHealthyNode("pool-1", nodeSetID, nodePoolID),
				makePoolHealthyNode("pool-2", nodeSetID, nodePoolID),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockTx := &testutil.MockTx{}
			mockTx.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
				return f.ClusterID != nil && *f.ClusterID == clusterID
			})).Return(tt.pools, nil)
			mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
				return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane
			})).Return(tt.nodes, nil)

			r := &reconciler{db: mockTx, logger: slog.Default()}
			cp := &controlPlane{clusterControlPlaneConfig: cluster.ControlPlaneConfig}
			got, err := r.isControlPlaneHealthy(context.Background(), cluster, cp, slog.Default())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			mockTx.AssertExpectations(t)
		})
	}
}

func makeSurgeHealthyNode() db.Node {
	node := makeHealthyNode("surge-1", uuid.Nil)
	node.NodePoolID = uuid.Nil
	node.ControlPlane = true
	return node
}

func makePoolHealthyNode(name string, nodeSetID, nodePoolID uuid.UUID) db.Node {
	node := makeHealthyNode(name, nodeSetID)
	node.NodePoolID = nodePoolID
	node.ControlPlane = true
	return node
}
