// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestNodePoolReadCounts(t *testing.T) {
	pool := db.NodePool{ID: uuid.New(), DesiredCount: 4}
	for _, endpoint := range []string{"get", "list"} {
		for _, fails := range []bool{false, true} {
			name := endpoint + "/success"
			if fails {
				name = endpoint + "/counts-error"
			}
			t.Run(name, func(t *testing.T) {
				store := &testutil.MockDB{}
				if endpoint == "get" {
					store.On("GetNodePool", mock.Anything, pool.ID).Return(pool, nil).Once()
				} else {
					store.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{pool}, nil).Once()
				}
				var queryErr error
				if fails {
					queryErr = errors.New("count query failed")
				}
				store.On("ListNodePoolCounts", mock.Anything, []db.NodePool{pool}).Return(map[uuid.UUID]db.NodePoolCounts{
					pool.ID: {NodeCount: 5, ReadyNodeCount: 3, UpdatedNodeCount: 2},
				}, queryErr).Once()
				svc := NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil)
				var result api.NodePool
				var err error
				if endpoint == "get" {
					var response api.GetNodePoolResponseObject
					response, err = svc.GetNodePool(t.Context(), api.GetNodePoolRequestObject{NodePoolID: pool.ID})
					if err == nil {
						result = api.NodePool(response.(api.GetNodePool200JSONResponse))
					}
				} else {
					var response api.ListNodePoolsResponseObject
					response, err = svc.ListNodePools(t.Context(), api.ListNodePoolsRequestObject{})
					if err == nil {
						result = response.(api.ListNodePools200JSONResponse).NodePools[0]
					}
				}
				if fails {
					require.ErrorContains(t, err, "count query failed")
				} else {
					require.NoError(t, err)
					require.Equal(t, 4, result.DesiredNodeCount)
					require.Equal(t, 5, *result.NodeCount)
					require.Equal(t, 3, *result.ReadyNodeCount)
					require.Equal(t, 2, *result.UpdatedNodeCount)
				}
				store.AssertExpectations(t)
			})
		}
	}
}

func TestNodePoolCountsRejectMissingObservation(t *testing.T) {
	store := &testutil.MockDB{}
	pool := db.NodePool{ID: uuid.New()}
	store.On("ListNodePoolCounts", mock.Anything, []db.NodePool{pool}).Return(map[uuid.UUID]db.NodePoolCounts{}, nil).Once()
	_, err := nodePoolCountsFromDB(t.Context(), store, []db.NodePool{pool})
	require.ErrorContains(t, err, "node pool counts missing")
	store.AssertExpectations(t)
}

func TestNodePoolWriteCounts(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		for _, fails := range []bool{false, true} {
			name := operation + "/success"
			if fails {
				name = operation + "/counts-error"
			}
			t.Run(name, func(t *testing.T) {
				cluster := forgeCluster()
				cluster.ID, cluster.Name, cluster.PodCIDR = uuid.New(), "tenant", "10.240.0.0/16"
				cluster.ServiceCIDR = "10.120.0.0/24"
				pool := db.NodePool{ID: uuid.New(), ClusterID: cluster.ID, DesiredCount: 4}
				tx := &testutil.MockTx{}
				store := &testutil.MockDB{MockTx: tx}
				store.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
				tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
				var write *mock.Call
				if operation == "create" {
					store.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
					tx.On("ListNodePools", mock.Anything, db.NodePoolFilters{ClusterID: &cluster.ID}).Return([]db.NodePool{}, nil).Once()
					write = tx.On("CreateNodePool", mock.Anything, mock.Anything).Return(pool, nil).Once()
					tx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil).Once()
				} else {
					tx.On("GetNodePool", mock.Anything, pool.ID).Return(pool, nil).Once()
					tx.On("GetNodePoolForUpdate", mock.Anything, pool.ID).Return(pool, nil).Once()
					write = tx.On("UpdateNodePool", mock.Anything, mock.Anything).Return(pool, nil).Once()
				}
				var queryErr error
				if fails {
					queryErr = errors.New("count query failed")
				}
				tx.On("ListNodePoolCounts", mock.Anything, []db.NodePool{pool}).Return(map[uuid.UUID]db.NodePoolCounts{
					pool.ID: {NodeCount: 5, ReadyNodeCount: 3, UpdatedNodeCount: 2},
				}, queryErr).Once().NotBefore(write)
				svc := NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil)
				var result api.NodePool
				var err error
				if operation == "create" {
					var response api.CreateNodePoolResponseObject
					response, err = svc.CreateNodePool(t.Context(), api.CreateNodePoolRequestObject{Body: &api.CreateNodePoolRequest{
						ClusterID: cluster.ID, Name: "workers", NodeNamePrefix: "w", DesiredNodeCount: 4,
						Nico: &api.NicoNodePoolProvider{InstanceTypeID: uuid.New(), SubnetIDs: []uuid.UUID{uuid.New()}, Release: "worker-release"},
					}})
					if err == nil {
						require.IsType(t, api.CreateNodePool201JSONResponse{}, response, "%#v", response)
						result = api.NodePool(response.(api.CreateNodePool201JSONResponse))
					}
				} else {
					var response api.UpdateNodePoolResponseObject
					response, err = svc.UpdateNodePool(t.Context(), api.UpdateNodePoolRequestObject{NodePoolID: pool.ID, Body: &api.UpdateNodePoolRequest{}})
					if err == nil {
						result = api.NodePool(response.(api.UpdateNodePool200JSONResponse))
					}
				}
				if fails {
					require.ErrorContains(t, err, "count query failed")
				} else {
					require.NoError(t, err)
					require.Equal(t, 5, *result.NodeCount)
					require.Equal(t, 3, *result.ReadyNodeCount)
					require.Equal(t, 2, *result.UpdatedNodeCount)
				}
				store.AssertExpectations(t)
				tx.AssertExpectations(t)
				store.AssertNotCalled(t, "ListNodePoolCounts", mock.Anything, mock.Anything)
			})
		}
	}
}

func TestListNodePoolsBatchCounts(t *testing.T) {
	pools := []db.NodePool{
		{ID: uuid.New(), Name: "first", SystemReservedMemory: "16Gi"},
		{ID: uuid.New(), Name: "second", SystemReservedMemory: "32Gi"},
	}
	store := &testutil.MockDB{}
	store.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return(pools, nil).Once()
	store.On("ListNodePoolCounts", mock.Anything, pools).Return(map[uuid.UUID]db.NodePoolCounts{
		pools[0].ID: {NodeCount: 5, ReadyNodeCount: 3, UpdatedNodeCount: 2},
		pools[1].ID: {NodeCount: 2, ReadyNodeCount: 1, UpdatedNodeCount: 0},
	}, nil).Once()
	service := NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil)
	response, err := service.ListNodePools(t.Context(), api.ListNodePoolsRequestObject{})
	require.NoError(t, err)
	responses := response.(api.ListNodePools200JSONResponse).NodePools
	require.Len(t, responses, 2)
	require.Equal(t, "first", responses[0].Name)
	require.Equal(t, "second", responses[1].Name)
	require.Equal(t, "16Gi", *responses[0].SystemReservedMemory)
	require.Equal(t, "32Gi", *responses[1].SystemReservedMemory)
	require.Equal(t, 5, *responses[0].NodeCount)
	require.Equal(t, 3, *responses[0].ReadyNodeCount)
	require.Equal(t, 2, *responses[0].UpdatedNodeCount)
	require.Equal(t, 2, *responses[1].NodeCount)
	require.Equal(t, 1, *responses[1].ReadyNodeCount)
	require.Zero(t, *responses[1].UpdatedNodeCount)
	store.AssertExpectations(t)
}

func TestListNodePoolsEmptyCounts(t *testing.T) {
	store := &testutil.MockDB{}
	store.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil).Once()
	service := NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil)
	response, err := service.ListNodePools(t.Context(), api.ListNodePoolsRequestObject{})
	require.NoError(t, err)
	pools := response.(api.ListNodePools200JSONResponse).NodePools
	require.NotNil(t, pools)
	require.Empty(t, pools)
	store.AssertNotCalled(t, "ListNodePoolCounts", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}
