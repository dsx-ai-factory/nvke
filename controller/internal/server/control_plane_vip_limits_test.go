// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
)

func TestNodePoolService_CreateControlPlanePoolWithoutVIPRejected(t *testing.T) {
	clusterID := uuid.New()
	existingPoolID := uuid.New()
	newPoolID := uuid.New()
	cluster := forgeDBCluster(db.Cluster{
		ID:          clusterID,
		Name:        "no-vip",
		PodCIDR:     "192.168.0.0/16",
		ServiceCIDR: "10.96.0.0/12",
		Nico:        &db.NicoClusterProvider{SiteID: uuid.New()},
	})

	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockDB.MockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{ClusterID: &clusterID}).Return([]db.NodePool{{
		ID:           existingPoolID,
		ClusterID:    clusterID,
		ControlPlane: true,
		DesiredCount: 1,
	}}, nil)
	mockDB.MockTx.On("CreateNodePool", mock.Anything, mock.Anything).Return(db.NodePool{
		ID:           newPoolID,
		ClusterID:    clusterID,
		ControlPlane: true,
		DesiredCount: 1,
	}, nil).Maybe()
	mockDB.MockTx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionNodePool", mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil).Maybe()

	service := server.NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, nil, false)
	resp, err := service.CreateNodePool(context.Background(), api.CreateNodePoolRequestObject{Body: &api.CreateNodePoolRequest{
		ClusterID:        clusterID,
		Name:             "second-cp",
		NodeNamePrefix:   "second-cp",
		ControlPlane:     true,
		DesiredNodeCount: 1,
		Nico:             testNicoNodePoolProvider(),
	}})
	require.NoError(t, err)

	badRequest, ok := resp.(api.CreateNodePool400JSONResponse)
	assert.True(t, ok, "expected CreateNodePool400JSONResponse, got %T", resp)
	if ok {
		assert.Contains(t, badRequest.Message, "controlPlaneVIP")
	}
	mockDB.MockTx.AssertNotCalled(t, "CreateNodePool", mock.Anything, mock.Anything)
}

func TestNodePoolService_UpdateDesiredCountWithoutVIP(t *testing.T) {
	for _, tc := range []struct {
		name             string
		controlPlane     bool
		controlPlaneVIP  string
		currentDesired   int
		requestedDesired int
		otherCPDesired   []int
		wantBadRequest   bool
		wantDesiredCount int
	}{
		{
			name:             "rejects control-plane scale-up",
			controlPlane:     true,
			currentDesired:   1,
			requestedDesired: 2,
			wantBadRequest:   true,
			wantDesiredCount: 1,
		},
		{
			name:             "rejects aggregate control-plane scale-up across pools",
			controlPlane:     true,
			currentDesired:   0,
			requestedDesired: 1,
			otherCPDesired:   []int{1},
			wantBadRequest:   true,
			wantDesiredCount: 0,
		},
		{
			name:             "allows unchanged control-plane capacity",
			controlPlane:     true,
			currentDesired:   1,
			requestedDesired: 1,
			wantDesiredCount: 1,
		},
		{
			name:             "allows control-plane scale-down for an existing cluster",
			controlPlane:     true,
			currentDesired:   2,
			requestedDesired: 1,
			wantDesiredCount: 1,
		},
		{
			name:             "allows historical multi-pool control-plane scale-down above one",
			controlPlane:     true,
			currentDesired:   2,
			requestedDesired: 1,
			otherCPDesired:   []int{1},
			wantDesiredCount: 1,
		},
		{
			name:             "allows control-plane scale-up with a VIP",
			controlPlane:     true,
			controlPlaneVIP:  "172.31.0.10",
			currentDesired:   1,
			requestedDesired: 2,
			wantDesiredCount: 2,
		},
		{
			name:             "allows worker scale-up",
			currentDesired:   1,
			requestedDesired: 2,
			wantDesiredCount: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clusterID := uuid.New()
			poolID := uuid.New()
			desired := tc.requestedDesired
			cluster := forgeDBCluster(db.Cluster{
				ID:              clusterID,
				Name:            "no-vip",
				ControlPlaneVIP: tc.controlPlaneVIP,
				PodCIDR:         "192.168.0.0/16",
				ServiceCIDR:     "10.96.0.0/12",
				Nico:            &db.NicoClusterProvider{SiteID: uuid.New()},
			})
			pool := db.NodePool{
				ID:           poolID,
				ClusterID:    clusterID,
				ControlPlane: tc.controlPlane,
				DesiredCount: tc.currentDesired,
				Nico:         &db.NicoNodePoolProvider{},
			}

			nodePools := make([]db.NodePool, 0, 1+len(tc.otherCPDesired))
			nodePools = append(nodePools, pool)
			for _, otherDesired := range tc.otherCPDesired {
				nodePools = append(nodePools, db.NodePool{
					ID:           uuid.New(),
					ClusterID:    clusterID,
					ControlPlane: true,
					DesiredCount: otherDesired,
				})
			}

			mockDB := &mockDB{MockTx: &mockTx{}}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			mockDB.MockTx.On("GetNodePool", mock.Anything, poolID).Return(pool, nil)
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
			mockDB.MockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
			mockDB.MockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{ClusterID: &clusterID}).Return(nodePools, nil)
			mockDB.MockTx.On("UpdateNodePool", mock.Anything, mock.Anything).Return(db.NodePool{
				ID:           poolID,
				ClusterID:    clusterID,
				ControlPlane: tc.controlPlane,
				DesiredCount: desired,
				Nico:         &db.NicoNodePoolProvider{},
			}, nil).Maybe()

			service := server.NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, nil, false)
			resp, err := service.UpdateNodePool(context.Background(), api.UpdateNodePoolRequestObject{
				NodePoolID: poolID,
				Body:       &api.UpdateNodePoolRequest{DesiredNodeCount: &desired},
			})
			require.NoError(t, err)

			if tc.wantBadRequest {
				badRequest, ok := resp.(api.UpdateNodePool400JSONResponse)
				assert.True(t, ok, "expected UpdateNodePool400JSONResponse, got %T", resp)
				if ok {
					assert.Contains(t, badRequest.Message, "controlPlaneVIP")
				}
				mockDB.MockTx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
				return
			}

			updated, ok := resp.(api.UpdateNodePool200JSONResponse)
			require.True(t, ok, "expected UpdateNodePool200JSONResponse, got %T", resp)
			assert.Equal(t, tc.wantDesiredCount, updated.DesiredNodeCount)
		})
	}
}

func TestNodePoolService_UpdateMaxSurgeDuringPendingRolloutWithoutVIP(t *testing.T) {
	clusterID := uuid.New()
	poolID := uuid.New()
	nodeSetID := uuid.New()
	maxSurge := 1
	cluster := forgeDBCluster(db.Cluster{
		ID:          clusterID,
		Name:        "no-vip",
		PodCIDR:     "192.168.0.0/16",
		ServiceCIDR: "10.96.0.0/12",
		Nico:        &db.NicoClusterProvider{SiteID: uuid.New()},
	})
	pool := db.NodePool{
		ID:               poolID,
		ClusterID:        clusterID,
		ControlPlane:     true,
		DesiredCount:     1,
		CurrentNodeSetID: &nodeSetID,
		Nico:             &db.NicoNodePoolProvider{ConfigProfile: "pending-config"},
	}

	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockDB.MockTx.On("GetNodePool", mock.Anything, poolID).Return(pool, nil)
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockDB.MockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
	mockDB.MockTx.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{
		ID:         nodeSetID,
		NodePoolID: poolID,
		Nico:       &db.NicoNodePoolProvider{},
	}, nil)
	mockDB.MockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{ClusterID: &clusterID}).Return([]db.NodePool{pool}, nil)
	mockDB.MockTx.On("UpdateNodePool", mock.Anything, mock.Anything).Return(db.NodePool{
		ID:           poolID,
		ClusterID:    clusterID,
		ControlPlane: true,
		DesiredCount: 1,
		MaxSurge:     maxSurge,
		Nico:         &db.NicoNodePoolProvider{},
	}, nil)

	service := server.NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, nil, false)
	resp, err := service.UpdateNodePool(context.Background(), api.UpdateNodePoolRequestObject{
		NodePoolID: poolID,
		Body:       &api.UpdateNodePoolRequest{MaxSurge: &maxSurge},
	})
	require.NoError(t, err)

	updated, ok := resp.(api.UpdateNodePool200JSONResponse)
	require.True(t, ok, "expected UpdateNodePool200JSONResponse, got %T", resp)
	require.NotNil(t, updated.MaxSurge)
	assert.Equal(t, maxSurge, *updated.MaxSurge)
}

func TestNodePoolService_UpdatePendingControlPlaneRolloutRequiresVIP(t *testing.T) {
	clusterID := uuid.New()
	poolID := uuid.New()
	nodeSetID := uuid.New()
	cluster := forgeDBCluster(db.Cluster{
		ID:          clusterID,
		Name:        "no-vip",
		PodCIDR:     "192.168.0.0/16",
		ServiceCIDR: "10.96.0.0/12",
		Nico:        &db.NicoClusterProvider{SiteID: uuid.New()},
	})
	pool := db.NodePool{
		ID:               poolID,
		ClusterID:        clusterID,
		ControlPlane:     true,
		DesiredCount:     1,
		CurrentNodeSetID: &nodeSetID,
		Nico:             &db.NicoNodePoolProvider{ConfigProfile: "pending-config"},
	}
	currentNodeSet := db.NodeSet{
		ID:         nodeSetID,
		NodePoolID: poolID,
		Nico:       &db.NicoNodePoolProvider{ConfigProfile: "current-config"},
	}
	newProfile := "next-config"

	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockDB.MockTx.On("GetNodePool", mock.Anything, poolID).Return(pool, nil)
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockDB.MockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
	mockDB.MockTx.On("GetNodeSet", mock.Anything, nodeSetID).Return(currentNodeSet, nil)

	service := server.NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, nil, false)
	resp, err := service.UpdateNodePool(context.Background(), api.UpdateNodePoolRequestObject{
		NodePoolID: poolID,
		Body: &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{ConfigProfile: &newProfile},
		},
	})

	require.NoError(t, err)
	badRequest, ok := resp.(api.UpdateNodePool400JSONResponse)
	require.True(t, ok, "expected UpdateNodePool400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "controlPlaneVIP")
	mockDB.MockTx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateLegacyMaxSurgeWithoutVIPAndConfigRevision(t *testing.T) {
	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                clusterID,
		Name:              "legacy-no-vip",
		KubernetesVersion: "1.34.2",
		ProviderType:      db.ProviderTypeCAPI,
		Capi:              &db.CAPIClusterProvider{},
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount: 1,
			MaxSurge:     1,
		},
	}
	maxSurge := 0
	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockDB.MockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(updated *db.Cluster) bool {
		return updated.ControlPlaneConfig.MaxSurge == maxSurge
	})).Return(cluster, nil)
	service := newTestClusterServiceWithControlPlaneController(slog.Default(), mockDB, false)

	resp, err := service.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body: &api.UpdateClusterRequest{ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			MaxSurge: &maxSurge,
		}},
	})

	require.NoError(t, err)
	require.IsType(t, api.UpdateCluster200JSONResponse{}, resp)
	mockDB.MockTx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
}

func TestNodePoolService_UpdateControlPlaneConfigRequiresVIP(t *testing.T) {
	for _, tc := range []struct {
		name            string
		controlPlaneVIP string
		wantBadRequest  bool
	}{
		{name: "without VIP", wantBadRequest: true},
		{name: "with VIP", controlPlaneVIP: "172.31.0.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clusterID := uuid.New()
			poolID := uuid.New()
			nodeSetID := uuid.New()
			cluster := forgeDBCluster(db.Cluster{
				ID:              clusterID,
				Name:            "control-plane-rollout",
				ControlPlaneVIP: tc.controlPlaneVIP,
				Nico:            &db.NicoClusterProvider{SiteID: uuid.New()},
			})
			pool := db.NodePool{
				ID:                   poolID,
				ClusterID:            clusterID,
				ControlPlane:         true,
				DesiredCount:         1,
				CurrentNodeSetID:     &nodeSetID,
				KubernetesVersion:    "1.34.2",
				SystemReservedMemory: "1Gi",
				Nico: &db.NicoNodePoolProvider{
					OSProfile:     "os-v1",
					ConfigProfile: "cfg-v1",
				},
			}
			currentNodeSet := db.NodeSet{
				ID:                   nodeSetID,
				NodePoolID:           poolID,
				KubernetesVersion:    pool.KubernetesVersion,
				SystemReservedMemory: pool.SystemReservedMemory,
				Nico: &db.NicoNodePoolProvider{
					OSProfile:     "os-v1",
					ConfigProfile: "cfg-v1",
				},
			}
			newProfile := "cfg-v2"

			mockDB := &mockDB{MockTx: &mockTx{}}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			mockDB.MockTx.On("GetNodePool", mock.Anything, poolID).Return(pool, nil)
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
			mockDB.MockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
			mockDB.MockTx.On("GetNodeSet", mock.Anything, nodeSetID).Return(currentNodeSet, nil).Maybe()
			updatedPool := pool
			updatedPool.Nico = &db.NicoNodePoolProvider{OSProfile: "os-v1", ConfigProfile: newProfile}
			updateCall := mockDB.MockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(got *db.NodePool) bool {
				return got.Nico != nil && got.Nico.ConfigProfile == newProfile
			})).Return(updatedPool, nil)
			if tc.wantBadRequest {
				updateCall.Maybe()
			} else {
				updateCall.Once()
			}

			service := server.NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, nil, false)
			resp, err := service.UpdateNodePool(context.Background(), api.UpdateNodePoolRequestObject{
				NodePoolID: poolID,
				Body: &api.UpdateNodePoolRequest{
					Nico: &api.NicoNodePoolProviderUpdate{ConfigProfile: &newProfile},
				},
			})
			require.NoError(t, err)

			if tc.wantBadRequest {
				badRequest, ok := resp.(api.UpdateNodePool400JSONResponse)
				require.True(t, ok, "expected UpdateNodePool400JSONResponse, got %T", resp)
				assert.Contains(t, badRequest.Message, "controlPlaneVIP")
				mockDB.MockTx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
				return
			}
			_, ok := resp.(api.UpdateNodePool200JSONResponse)
			require.True(t, ok, "expected UpdateNodePool200JSONResponse, got %T", resp)
			mockDB.MockTx.AssertExpectations(t)
		})
	}
}
