// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controlplane

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
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

func TestIsSurgeAtClusterLevel_InstanceTypePreferenceNotRequired(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{
		ControlPlaneConfig: db.ControlPlaneConfig{
			MaxSurge: 1,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				InstanceTypePreference: []uuid.UUID{uuid.New()},
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

	withoutSurge, err := r.getTotalDesiredControlPlaneNodes(ctx, cluster, false)
	require.NoError(t, err)
	assert.Equal(t, 3, withoutSurge)

	withSurge, err := r.getTotalDesiredControlPlaneNodes(ctx, cluster, true)
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
			got, err := r.isControlPlaneHealthy(context.Background(), cluster, slog.Default())
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
