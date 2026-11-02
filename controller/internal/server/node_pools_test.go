// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// testNicoNodePoolProvider is the nested Forge payload the read fixtures below
// store, as it comes back on read.
func testNicoNodePoolProvider() *api.NicoNodePoolProvider {
	return &api.NicoNodePoolProvider{
		SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
		InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
		SSHKeyGroupIDs: &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
		Release:        "ubuntu",
		ConfigProfile:  "default",
	}
}

func TestNodePoolService_ListNodePools(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	now := time.Now()
	tests := []struct {
		name         string
		filters      db.NodePoolFilters
		dbNodePools  []db.NodePool
		dbError      error
		wantError    bool
		wantNodePool []api.NodePool
	}{
		{
			name: "success with cluster filter",
			filters: db.NodePoolFilters{
				ClusterID: &clusterID,
			},
			dbNodePools: []db.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}},
					Conditions:           mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
						InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
						SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
						Release:        "ubuntu",
						ConfigProfile:  "default",
					},
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					Nico:                 testNicoNodePoolProvider(),
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
					Conditions:           mockAPINodePoolConditions(now),
					TeleportEnabled:      testutil.PtrTo(false),
				},
			},
		},
		{
			name:    "success without filter",
			filters: db.NodePoolFilters{},
			dbNodePools: []db.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}},
					Conditions:           mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
						InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
						SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
						Release:        "ubuntu",
						ConfigProfile:  "default",
					},
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					Nico:                 testNicoNodePoolProvider(),
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
					Conditions:           mockAPINodePoolConditions(now),
					TeleportEnabled:      testutil.PtrTo(false),
				},
			},
		},
		{
			name: "success with name filter",
			filters: db.NodePoolFilters{
				Name: func() *string { s := "test-pool"; return &s }(),
			},
			dbNodePools: []db.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}},
					Conditions:           mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
						InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
						SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
						Release:        "ubuntu",
						ConfigProfile:  "default",
					},
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					Nico:                 testNicoNodePoolProvider(),
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
					Conditions:           mockAPINodePoolConditions(now),
					CurrentNodeSetID:     nil,
					TeleportEnabled:      testutil.PtrTo(false),
				},
			},
		},
		{
			name: "success with cluster name filter",
			filters: db.NodePoolFilters{
				ClusterName: func() *string { s := "test-cluster"; return &s }(),
			},
			dbNodePools: []db.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}},
					Conditions:           mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
						InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
						SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
						Release:        "ubuntu",
						ConfigProfile:  "default",
					},
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					Nico:                 testNicoNodePoolProvider(),
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
					NodeTaints:           []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
					Conditions:           mockAPINodePoolConditions(now),
					CurrentNodeSetID:     nil,
					TeleportEnabled:      testutil.PtrTo(false),
				},
			},
		},
		{
			name:      "db error",
			filters:   db.NodePoolFilters{},
			dbError:   errors.New("db error"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.On("ListNodePools", mock.Anything, tt.filters).Return(tt.dbNodePools, tt.dbError)

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, config.TeleportConfig{}, nil)

			resp, err := service.ListNodePools(context.Background(), api.ListNodePoolsRequestObject{
				Params: api.ListNodePoolsParams{
					ClusterID:   tt.filters.ClusterID,
					Name:        tt.filters.Name,
					ClusterName: tt.filters.ClusterName,
				},
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			response := resp.(api.ListNodePools200JSONResponse)
			assert.Equal(t, tt.wantNodePool, response.NodePools)
			mockDB.AssertExpectations(t)
		})
	}
}

func TestNodePoolService_CreateNodePool(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	requestCtx, requestSpan := provider.Tracer("test").Start(t.Context(), "create-node-pool")
	defer requestSpan.End()
	wantTraceID := requestSpan.SpanContext().TraceID()
	requestTrace := mock.MatchedBy(func(ctx context.Context) bool {
		spanContext := trace.SpanContextFromContext(ctx)
		return spanContext.TraceID() == wantTraceID && spanContext.SpanID().IsValid()
	})

	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	siteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	orgID := testOrgID // Use the same test orgID from clusters_test.go
	tests := []struct {
		name                  string
		request               api.CreateNodePoolRequest
		setupMockCalls        func(*mockTx)
		wantResponseError     bool
		responseStatus        int
		responseErrorContains string
		wantError             bool
		errorContains         string
	}{
		{
			name: "success",
			request: func() api.CreateNodePoolRequest {
				nvLinkLogicalPartitionID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440006")
				return api.CreateNodePoolRequest{
					ClusterID:      clusterID,
					Name:           "test-pool",
					NodeNamePrefix: "test-pool-prefix",
					Nico: &api.NicoNodePoolProvider{
						SubnetIDs:                []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
						InstanceTypeID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
						NvLinkLogicalPartitionID: &nvLinkLogicalPartitionID,
						SSHKeyGroupIDs:           &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
						Release:                  "ubuntu",
						ConfigProfile:            "default",
					},
					NodeLabels:         map[string]string{"label1": "value1", "label2": "value2"},
					MachineAnnotations: map[string]string{"example.com/profiles": "gpu,network"},
					MachineLabels:      map[string]string{"example.com/pool": "gpu"},
					NodeTaints:         []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
					DesiredNodeCount:   3,
				}
			}(),
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					nico, err := n.NicoProvider()
					return err == nil &&
						n.Name == "test-pool" &&
						n.NodeNamePrefix == "test-pool-prefix" &&
						assert.ObjectsAreEqual(map[string]string{"label1": "value1", "label2": "value2"}, n.NodeLabels) &&
						assert.ObjectsAreEqual(map[string]string{"example.com/profiles": "gpu,network"}, n.MachineAnnotations) &&
						assert.ObjectsAreEqual(map[string]string{"example.com/pool": "gpu"}, n.MachineLabels) &&
						assert.ObjectsAreEqual([]db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}}, n.NodeTaints) &&
						nico.NvLinkLogicalPartitionID != nil &&
						*nico.NvLinkLogicalPartitionID == uuid.MustParse("550e8400-e29b-41d4-a716-446655440006")
				})).Return(db.NodePool{
					ID:             nodePoolID,
					Name:           "test-pool",
					NodeNamePrefix: "test-pool-prefix",
				}, nil)

				tx.On("CreateWorkflowOutboxEntry", requestTrace, "ProvisionNodePool", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
					m, ok := input.(map[string]interface{})
					return ok && m["nodePoolID"] == nodePoolID
				})).Return(db.WorkflowOutbox{}, nil)
			},
		},
		{
			name: "pod CIDR lacks capacity for new node pool",
			request: api.CreateNodePoolRequest{
				ClusterID:        clusterID,
				Name:             "test-pool",
				NodeNamePrefix:   "test-pool-prefix",
				Nico:             testNicoNodePoolProvider(),
				DesiredNodeCount: 2,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					PodCIDR:     "10.240.0.0/23",
					ServiceCIDR: "10.120.0.0/24",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}, nil)
				tx.On(
					"ListNodePools",
					mock.Anything,
					db.NodePoolFilters{ClusterID: &clusterID},
				).Return([]db.NodePool{}, nil)
			},
			wantResponseError:     true,
			responseStatus:        http.StatusBadRequest,
			responseErrorContains: "provides 2 /24 node blocks, but 3 are required",
		},
		{
			name: "duplicate node pool name in the same cluster",
			request: api.CreateNodePoolRequest{
				ClusterID:        clusterID,
				Name:             "test-pool",
				NodeNamePrefix:   "test-pool-prefix",
				Nico:             testNicoNodePoolProvider(),
				NodeLabels:       map[string]string{"label1": "value1", "label2": "value2"},
				NodeTaints:       []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
				DesiredNodeCount: 3,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					return n.Name == "test-pool" &&
						n.NodeNamePrefix == "test-pool-prefix" &&
						assert.ObjectsAreEqual(map[string]string{"label1": "value1", "label2": "value2"}, n.NodeLabels) &&
						assert.ObjectsAreEqual([]db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}}, n.NodeTaints)
				})).Return(db.NodePool{}, db.ErrNodePoolNameNotUniqueInCluster)
			},
			wantResponseError: true,
			// response is json so the quotes are escaped
			responseErrorContains: "A node pool already exists with the name \\\"test-pool\\\" in cluster ID \\\"" + clusterID.String() + "\\\"",
		},
		{
			name: "node pool creation error",
			request: api.CreateNodePoolRequest{
				ClusterID:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				Name:             "test-pool",
				NodeNamePrefix:   "test-pool-prefix",
				Nico:             testNicoNodePoolProvider(),
				DesiredNodeCount: 3,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.Anything).Return(db.NodePool{}, errors.New("create node pool error"))
			},
			wantError: true,
		},
		{
			name: "success with VPC prefixes (native networking enabled)",
			request: api.CreateNodePoolRequest{
				ClusterID:      clusterID,
				Name:           "test-pool-vpc",
				NodeNamePrefix: "test-pool-vpc-prefix",
				Nico: &api.NicoNodePoolProvider{
					VpcPrefixIDs:   []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440005")},
					InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs: &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					Release:        "ubuntu",
					ConfigProfile:  "default",
				},
				NodeLabels:       map[string]string{"label1": "value1", "label2": "value2"},
				NodeTaints:       []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
				DesiredNodeCount: 3,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					nico, err := n.NicoProvider()
					return err == nil &&
						n.Name == "test-pool-vpc" &&
						n.NodeNamePrefix == "test-pool-vpc-prefix" &&
						len(nico.VpcPrefixIDs) > 0 &&
						assert.ObjectsAreEqual(map[string]string{"label1": "value1", "label2": "value2"}, n.NodeLabels) &&
						assert.ObjectsAreEqual([]db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}}, n.NodeTaints)
				})).Return(db.NodePool{
					ID:             nodePoolID,
					Name:           "test-pool-vpc",
					NodeNamePrefix: "test-pool-vpc-prefix",
				}, nil)

				tx.On("CreateWorkflowOutboxEntry", requestTrace, "ProvisionNodePool", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
					m, ok := input.(map[string]interface{})
					return ok && m["nodePoolID"] == nodePoolID
				})).Return(db.WorkflowOutbox{}, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}

			// Mock GetCluster on mockDB for validation (validation uses s.db, not the transaction)
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
				ID:           clusterID,
				ProviderType: db.ProviderTypeNico,
				PodCIDR:      "192.168.0.0/16",
				ServiceCIDR:  "10.96.0.0/12",
				Nico: &db.NicoClusterProvider{
					OrgID:  orgID,
					SiteID: siteID,
				},
			}, nil)

			if tt.setupMockCalls != nil {
				tt.setupMockCalls(mockDB.MockTx)
			}
			mockDB.MockTx.On(
				"ListNodePools",
				mock.Anything,
				db.NodePoolFilters{ClusterID: &clusterID},
			).Return([]db.NodePool{}, nil).Maybe()

			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, config.TeleportConfig{}, nil)

			resp, err := service.CreateNodePool(requestCtx, api.CreateNodePoolRequestObject{
				Body: &tt.request,
			})

			if tt.wantError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)

			responseWriter := httptest.NewRecorder()
			require.NoError(t, resp.VisitCreateNodePoolResponse(responseWriter))
			response := responseWriter.Result()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			if tt.responseStatus != 0 {
				assert.Equal(t, tt.responseStatus, response.StatusCode)
			}

			if tt.wantResponseError {
				assert.Contains(t, string(body), tt.responseErrorContains)
			}

			mockDB.AssertExpectations(t)
			mockDB.MockTx.AssertExpectations(t)
		})
	}
}

func TestNodePoolService_GetNodePool(t *testing.T) {
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	now := time.Now()
	tests := []struct {
		name         string
		nodePoolID   uuid.UUID
		dbNodePool   db.NodePool
		dbError      error
		wantError    bool
		want404      bool
		wantNodePool api.GetNodePool200JSONResponse
	}{
		{
			name:       "success",
			nodePoolID: nodePoolID,
			dbNodePool: db.NodePool{
				ID:                   nodePoolID,
				ClusterID:            uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				Name:                 "test-pool",
				NodeNamePrefix:       "test-pool",
				SystemReservedMemory: "16Gi",
				ControlPlane:         false,
				NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
				NodeTaints:           []db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}},
				Conditions:           mockDBNodePoolConditions(now),
				Nico: &db.NicoNodePoolProvider{
					SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					Release:        "ubuntu",
					ConfigProfile:  "default",
				},
			},
			wantNodePool: api.GetNodePool200JSONResponse{
				ID:                   nodePoolID,
				ClusterID:            uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				Name:                 "test-pool",
				NodeNamePrefix:       "test-pool",
				Nico:                 testNicoNodePoolProvider(),
				SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
				ControlPlane:         false,
				NodeLabels:           map[string]string{"label1": "value1", "label2": "value2"},
				NodeTaints:           []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
				Conditions:           mockAPINodePoolConditions(now),
				TeleportEnabled:      testutil.PtrTo(false),
			},
		},
		{
			name:       "not found",
			nodePoolID: nodePoolID,
			dbError:    db.ErrNotFound,
			want404:    true,
		},
		{
			name:       "db error",
			nodePoolID: nodePoolID,
			dbError:    errors.New("db error"),
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.On("GetNodePool", mock.Anything, tt.nodePoolID).Return(tt.dbNodePool, tt.dbError)

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, config.TeleportConfig{}, nil)

			resp, err := service.GetNodePool(context.Background(), api.GetNodePoolRequestObject{
				NodePoolID: tt.nodePoolID,
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}
			if tt.want404 {
				assert.IsType(t, api.GetNodePool404Response{}, resp)
				return
			}

			require.NoError(t, err)
			response := resp.(api.GetNodePool200JSONResponse)
			assert.Equal(t, tt.wantNodePool, response)
			mockDB.AssertExpectations(t)
		})
	}
}

func TestNodePoolService_UpdateNodePool(t *testing.T) {
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	siteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
	subnetID1 := uuid.MustParse("550e8400-e29b-41d4-a716-446655440010")
	subnetID2 := uuid.MustParse("550e8400-e29b-41d4-a716-446655440011")
	vpcPrefixID1 := uuid.MustParse("550e8400-e29b-41d4-a716-446655440020")
	vpcPrefixID2 := uuid.MustParse("550e8400-e29b-41d4-a716-446655440021")
	currentNodeSetID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440030")
	now := time.Now()

	tests := []struct {
		name             string
		nodePoolID       uuid.UUID
		body             api.UpdateNodePoolRequest
		setupMockDB      func(*mockDB, *mockTx)
		expectError      bool        // if true, assert resp is nil and err != nil; else assert no error and validate response
		wantResponseType interface{} // expected response type when no error (e.g. api.UpdateNodePool200JSONResponse{})
		verifyUpdatePool func(t *testing.T, n *db.NodePool)
	}{
		{
			name:       "success with subnetIDs replace (differs) only updates pool; controller creates NodeSet on reconcile",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{subnetID2}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:               nodePoolID,
					ClusterID:        clusterID,
					CurrentNodeSetID: &currentNodeSetID,
					Conditions:       mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:               nodePoolID,
					CurrentNodeSetID: &currentNodeSetID,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID2},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{subnetID2}, nico.SubnetIDs)
				assert.Equal(t, currentNodeSetID, *n.CurrentNodeSetID)
			},
		},
		{
			name:       "success with subnetIDs replace same set (no new NodeSet)",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{subnetID1}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID: nodePoolID,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{subnetID1}, nico.SubnetIDs)
			},
		},
		{
			name:       "empty subnetIDs replaces subnets with empty",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico:             &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{}},
				DesiredNodeCount: testutil.PtrTo(3),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					DesiredCount: 3,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Empty(t, nico.SubnetIDs)
				assert.Equal(t, 3, n.DesiredCount)
			},
		},
		{
			name:       "nil subnetIDs (field omitted) leaves subnets unchanged",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				DesiredNodeCount: testutil.PtrTo(4),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1, subnetID2},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					DesiredCount: 4,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1, subnetID2},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, []uuid.UUID{subnetID1, subnetID2}, nico.SubnetIDs)
				assert.Equal(t, 4, n.DesiredCount)
			},
		},
		{
			name:       "success without subnetIDs leaves subnets unchanged",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				DesiredNodeCount: testutil.PtrTo(5),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					DesiredCount: 5,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{subnetID1}, nico.SubnetIDs)
				assert.Equal(t, 5, n.DesiredCount)
			},
		},
		{
			name:       "subnetIDs when node pool not found returns 404",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{subnetID1}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound)
			},
			wantResponseType: api.UpdateNodePool404Response{},
		},
		{
			name:       "subnetIDs when cluster not found returns 404",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{subnetID1}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				// Cluster-first order: the cluster lock fails before the pool
				// row is locked, so GetNodePoolForUpdate is never reached.
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)
			},
			wantResponseType: api.UpdateNodePool404Response{},
		},
		{
			name:       "success with vpcPrefixIDs replace (differs) only updates pool; controller creates NodeSet on reconcile",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID2}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:               nodePoolID,
					ClusterID:        clusterID,
					CurrentNodeSetID: &currentNodeSetID,
					Conditions:       mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:               nodePoolID,
					CurrentNodeSetID: &currentNodeSetID,
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID2},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{vpcPrefixID2}, nico.VpcPrefixIDs)
				assert.Equal(t, currentNodeSetID, *n.CurrentNodeSetID)
			},
		},
		{
			name:       "success with vpcPrefixIDs replace same set (no new NodeSet)",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID: nodePoolID,
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, []uuid.UUID{vpcPrefixID1}, nico.VpcPrefixIDs)
			},
		},
		{
			name:       "empty vpcPrefixIDs replaces vpc prefixes with empty",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico:             &api.NicoNodePoolProviderUpdate{VpcPrefixIDs: []openapi_types.UUID{}},
				DesiredNodeCount: testutil.PtrTo(2),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					DesiredCount: 2,
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Empty(t, nico.VpcPrefixIDs)
				assert.Equal(t, 2, n.DesiredCount)
			},
		},
		{
			name:       "nil vpcPrefixIDs (field omitted) leaves vpc prefixes unchanged",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				DesiredNodeCount: testutil.PtrTo(6),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1, vpcPrefixID2},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					DesiredCount: 6,
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1, vpcPrefixID2},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, []uuid.UUID{vpcPrefixID1, vpcPrefixID2}, nico.VpcPrefixIDs)
				assert.Equal(t, 6, n.DesiredCount)
			},
		},
		{
			name:       "subnetIDs replace when no current NodeSet only updates pool",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{subnetID2}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID: nodePoolID,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID2},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, []uuid.UUID{subnetID2}, nico.SubnetIDs)
				assert.Nil(t, n.CurrentNodeSetID)
			},
		},
		{
			name:       "subnetIDs dedupe and same set no new NodeSet",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []openapi_types.UUID{subnetID1, subnetID1, subnetID2}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID2, subnetID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID: nodePoolID,
					Nico: &db.NicoNodePoolProvider{
						SubnetIDs: []uuid.UUID{subnetID1, subnetID2},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				require.Len(t, nico.SubnetIDs, 2)
				assert.Contains(t, nico.SubnetIDs, subnetID1)
				assert.Contains(t, nico.SubnetIDs, subnetID2)
			},
		},
		{
			name:       "success without vpcPrefixIDs leaves vpc prefixes unchanged",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				DesiredNodeCount: testutil.PtrTo(5),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					Conditions: mockDBNodePoolConditions(now),
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					DesiredCount: 5,
					Nico: &db.NicoNodePoolProvider{
						VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					},
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				nico, err := n.NicoProvider()
				require.NoError(t, err)
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{vpcPrefixID1}, nico.VpcPrefixIDs)
				assert.Equal(t, 5, n.DesiredCount)
			},
		},
		{
			name:       "both subnetIDs and vpcPrefixIDs returns 400",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{
					SubnetIDs:    []openapi_types.UUID{subnetID1},
					VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1},
				},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
			},
			wantResponseType: api.UpdateNodePool400JSONResponse{},
		},
		{
			name:       "vpcPrefixIDs when node pool not found returns 404",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound)
			},
			wantResponseType: api.UpdateNodePool404Response{},
		},
		{
			name:       "vpcPrefixIDs when cluster not found returns 404",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				Nico: &api.NicoNodePoolProviderUpdate{VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1}},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				// Cluster-first order: the cluster lock fails before the pool
				// row is locked, so GetNodePoolForUpdate is never reached.
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)
			},
			wantResponseType: api.UpdateNodePool404Response{},
		},
		{
			name:       "node pool deleting returns error",
			nodePoolID: nodePoolID,
			body:       api.UpdateNodePoolRequest{},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				deletingConditions := mockDBNodePoolConditions(now)
				deletingConditions.Deleting = db.Condition{Status: db.ConditionStatusTrue, Message: "Deleting"}
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					Conditions: deletingConditions,
				}, nil)
			},
			expectError: true,
		},
		{
			name:       "tx validation failure returns 400 response",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				MaxSurge: testutil.PtrTo(2),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:           nodePoolID,
					ClusterID:    clusterID,
					ControlPlane: true,
					Conditions:   mockDBNodePoolConditions(now),
				}, nil)
			},
			wantResponseType: api.UpdateNodePool400JSONResponse{},
		},
		{
			name:       "node pool not found in tx returns 404",
			nodePoolID: nodePoolID,
			body:       api.UpdateNodePoolRequest{},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound)
			},
			wantResponseType: api.UpdateNodePool404Response{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}
			if tt.setupMockDB != nil {
				tt.setupMockDB(mockDB, mockDB.MockTx)
			}
			// Unlocked read used to learn the pool's cluster before taking the
			// cluster-first lock order. UpdateNodePool always performs this
			// pre-read first, so require it (Once) rather than Maybe so a
			// regression that drops it fails the test.
			mockDB.MockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
				ID:        nodePoolID,
				ClusterID: clusterID,
			}, nil).Maybe()
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Maybe().Return(db.Cluster{
				ID:           clusterID,
				ProviderType: db.ProviderTypeNico,
				PodCIDR:      "192.168.0.0/16",
				ServiceCIDR:  "10.96.0.0/12",
				Nico: &db.NicoClusterProvider{
					OrgID:  testOrgID,
					SiteID: siteID,
				},
			}, nil)
			mockDB.MockTx.On(
				"ListNodePools",
				mock.Anything,
				db.NodePoolFilters{ClusterID: &clusterID},
			).Maybe().Return([]db.NodePool{{ID: nodePoolID}}, nil)
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Maybe().Return(nil)

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, config.TeleportConfig{}, nil)

			resp, err := service.UpdateNodePool(context.Background(), api.UpdateNodePoolRequestObject{
				NodePoolID: tt.nodePoolID,
				Body:       &tt.body,
			})

			if tt.expectError {
				assert.Nil(t, resp)
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			require.NotNil(t, tt.wantResponseType, "test must set wantResponseType when expectError is false")
			assert.IsType(t, tt.wantResponseType, resp)
			if tt.verifyUpdatePool != nil {
				validated := false
				for _, call := range mockDB.MockTx.Calls {
					if call.Method == "UpdateNodePool" {
						tt.verifyUpdatePool(t, call.Arguments[1].(*db.NodePool))
						validated = true
						break
					}
				}
				assert.True(t, validated, "UpdateNodePool was not called")
			}

			mockDB.AssertExpectations(t)
			mockDB.MockTx.AssertExpectations(t)
		})
	}
}

func TestNodePoolService_UpdateNodePool_RejectsPodCIDRCapacityExceeded(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	desiredNodeCount := 2

	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.MockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
		ID:        nodePoolID,
		ClusterID: clusterID,
	}, nil)
	mockDB.MockTx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
		ID:           nodePoolID,
		ClusterID:    clusterID,
		DesiredCount: 1,
	}, nil)
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(forgeDBCluster(db.Cluster{
		ID:          clusterID,
		PodCIDR:     "10.240.0.0/23",
		ServiceCIDR: "10.120.0.0/24",
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.New(),
		},
	}), nil)
	mockDB.MockTx.On(
		"ListNodePools",
		mock.Anything,
		db.NodePoolFilters{ClusterID: &clusterID},
	).Return([]db.NodePool{{
		ID:           nodePoolID,
		ClusterID:    clusterID,
		DesiredCount: 1,
	}}, nil)
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	service := server.NewNodePoolService(&slog.Logger{}, mockDB, config.TeleportConfig{}, nil)
	resp, err := service.UpdateNodePool(context.Background(), api.UpdateNodePoolRequestObject{
		NodePoolID: nodePoolID,
		Body: &api.UpdateNodePoolRequest{
			DesiredNodeCount: &desiredNodeCount,
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateNodePool400JSONResponse)
	require.True(t, ok, "expected UpdateNodePool400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "provides 2 /24 node blocks, but 3 are required")
	mockDB.MockTx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
}

func TestNodePoolService_DeleteNodePool(t *testing.T) {
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	otherCPPoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440010")
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	joinedCPNodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440020")
	tests := []struct {
		name             string
		nodePoolID       uuid.UUID
		setupMockCalls   func(*mockTx)
		wantError        bool
		want400          bool
		want404          bool
		want400Contains  string
		verifyConditions func(t *testing.T, conditions db.NodePoolConditions)
	}{
		{
			name:       "success worker pool",
			nodePoolID: nodePoolID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:           nodePoolID,
					ClusterID:    clusterID,
					ControlPlane: false,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					return n.ID == nodePoolID && n.Conditions.Deleting.Status == db.ConditionStatusTrue
				})).Return(db.NodePool{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "TeardownNodePool", mock.Anything,
					mock.MatchedBy(func(input interface{}) bool {
						m, ok := input.(map[string]interface{})
						return ok && m["nodePoolID"] == nodePoolID
					})).Return(db.WorkflowOutbox{}, nil)
			},
			verifyConditions: func(t *testing.T, conditions db.NodePoolConditions) {
				t.Helper()
				assert.Equal(t, db.ConditionStatusTrue, conditions.Deleting.Status)
			},
		},
		{
			name:       "success control-plane pool when another joined CP remains",
			nodePoolID: nodePoolID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:           nodePoolID,
					ClusterID:    clusterID,
					ControlPlane: true,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				controlPlane := true
				tx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.NodePool{
					{ID: nodePoolID, ControlPlane: true},
					{ID: otherCPPoolID, ControlPlane: true},
				}, nil)
				tx.On("ListNodes", mock.Anything, db.NodeFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.Node{
					{
						ID:           joinedCPNodeID,
						NodePoolID:   otherCPPoolID,
						ControlPlane: true,
						Conditions: db.NodeConditions{
							Joined: db.NewCondition(true, "joined"),
						},
					},
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					return n.ID == nodePoolID && n.Conditions.Deleting.Status == db.ConditionStatusTrue
				})).Return(db.NodePool{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "TeardownNodePool", mock.Anything,
					mock.MatchedBy(func(input interface{}) bool {
						m, ok := input.(map[string]interface{})
						return ok && m["nodePoolID"] == nodePoolID
					})).Return(db.WorkflowOutbox{}, nil)
			},
		},
		{
			name:       "reject last joined control-plane pool",
			nodePoolID: nodePoolID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:           nodePoolID,
					ClusterID:    clusterID,
					ControlPlane: true,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				controlPlane := true
				tx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.NodePool{
					{ID: nodePoolID, ControlPlane: true},
				}, nil)
				tx.On("ListNodes", mock.Anything, db.NodeFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.Node{
					{
						ID:           joinedCPNodeID,
						NodePoolID:   nodePoolID,
						ControlPlane: true,
						Conditions: db.NodeConditions{
							Joined: db.NewCondition(true, "joined"),
						},
					},
				}, nil)
			},
			want400:         true,
			want400Contains: "last joined control-plane node pool",
		},
		{
			name:       "noop while cluster deleting",
			nodePoolID: nodePoolID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
					Conditions: db.ClusterConditions{
						Deleting: db.NewCondition(true, "deleting"),
					},
				}, nil)
			},
		},
		{
			name:       "not found",
			nodePoolID: nodePoolID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound)
			},
			want404: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}

			if tt.setupMockCalls != nil {
				tt.setupMockCalls(mockDB.MockTx)
			}

			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, config.TeleportConfig{}, nil)

			resp, err := service.DeleteNodePool(context.Background(), api.DeleteNodePoolRequestObject{
				NodePoolID: tt.nodePoolID,
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}
			if tt.want400 {
				require.IsType(t, api.DeleteNodePool400JSONResponse{}, resp)
				badRequest := resp.(api.DeleteNodePool400JSONResponse)
				assert.Contains(t, badRequest.Message, tt.want400Contains)
				return
			}
			if tt.want404 {
				assert.IsType(t, api.DeleteNodePool404Response{}, resp)
				return
			}

			require.NoError(t, err)
			require.IsType(t, api.DeleteNodePool202Response{}, resp)

			if tt.verifyConditions != nil {
				var conditions db.NodePoolConditions
				for _, call := range mockDB.MockTx.Calls {
					if call.Method == "UpdateNodePool" {
						nodePool := call.Arguments[1].(*db.NodePool)
						conditions = nodePool.Conditions
						break
					}
				}
				tt.verifyConditions(t, conditions)
			}

			mockDB.AssertExpectations(t)
			mockDB.MockTx.AssertExpectations(t)
		})
	}
}

func mockDBNodePoolConditions(now time.Time) db.NodePoolConditions {
	return db.NodePoolConditions{
		Provisioning: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Finished provisioning",
			LastTransitionTime: now,
		},
		Provisioned: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "Provisioned",
			LastTransitionTime: now,
		},
		Deleting: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Not deleting",
			LastTransitionTime: now,
		},
	}
}

func mockAPINodePoolConditions(now time.Time) api.NodePoolConditions {
	return api.NodePoolConditions{
		Provisioning: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Finished provisioning",
			LastTransitionTime: now,
		},
		Provisioned: api.Condition{
			Status:             api.ConditionStatusTrue,
			Message:            "Provisioned",
			LastTransitionTime: now,
		},
		Deleting: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Not deleting",
			LastTransitionTime: now,
		},
	}
}
