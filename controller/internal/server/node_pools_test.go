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

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/testutil"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/server"
)

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
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
					Conditions:           mockDBNodePoolConditions(now),
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
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
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
					Conditions:           mockDBNodePoolConditions(now),
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
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
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
					Conditions:           mockDBNodePoolConditions(now),
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
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
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: "16Gi",
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
					Conditions:           mockDBNodePoolConditions(now),
				},
			},
			wantNodePool: []api.NodePool{
				{
					ID:                   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					ClusterID:            clusterID,
					Name:                 "test-pool",
					NodeNamePrefix:       "test-pool",
					SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:            "ubuntu",
					ConfigProfile:        "default",
					SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
					ControlPlane:         false,
					NodeLabels:           []string{"label1", "label2"},
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

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, nil, config.TeleportConfig{}, false)

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
					ClusterID:                clusterID,
					Name:                     "test-pool",
					NodeNamePrefix:           "test-pool-prefix",
					SubnetIDs:                []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
					InstanceTypeID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
					NvLinkLogicalPartitionID: &nvLinkLogicalPartitionID,
					SSHKeyGroupIDs:           &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
					OSProfile:                "ubuntu",
					ConfigProfile:            "default",
					NodeLabels:               &[]string{"label1", "label2"},
					DesiredNodeCount:         3,
				}
			}(),
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					OrgID:       orgID,
					SiteID:      siteID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					return n.Name == "test-pool" &&
						n.NodeNamePrefix == "test-pool-prefix" &&
						n.NvLinkLogicalPartitionID != nil &&
						*n.NvLinkLogicalPartitionID == uuid.MustParse("550e8400-e29b-41d4-a716-446655440006")
				})).Return(db.NodePool{
					ID:             nodePoolID,
					Name:           "test-pool",
					NodeNamePrefix: "test-pool-prefix",
				}, nil)

				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionNodePool", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
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
				SubnetIDs:        []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
				InstanceTypeID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
				SSHKeyGroupIDs:   &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
				OSProfile:        "ubuntu",
				ConfigProfile:    "default",
				DesiredNodeCount: 2,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					OrgID:       orgID,
					SiteID:      siteID,
					PodCIDR:     "10.240.0.0/23",
					ServiceCIDR: "10.120.0.0/24",
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
				SubnetIDs:        []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
				InstanceTypeID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
				SSHKeyGroupIDs:   &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
				OSProfile:        "ubuntu",
				ConfigProfile:    "default",
				NodeLabels:       &[]string{"label1", "label2"},
				DesiredNodeCount: 3,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					OrgID:       orgID,
					SiteID:      siteID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					return n.Name == "test-pool" && n.NodeNamePrefix == "test-pool-prefix"
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
				SubnetIDs:        []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
				InstanceTypeID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
				SSHKeyGroupIDs:   &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
				OSProfile:        "ubuntu",
				ConfigProfile:    "default",
				DesiredNodeCount: 3,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					OrgID:       orgID,
					SiteID:      siteID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.Anything).Return(db.NodePool{}, errors.New("create node pool error"))
			},
			wantError: true,
		},
		{
			name: "success with VPC prefixes (native networking enabled)",
			request: api.CreateNodePoolRequest{
				ClusterID:        clusterID,
				Name:             "test-pool-vpc",
				NodeNamePrefix:   "test-pool-vpc-prefix",
				VpcPrefixIDs:     []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440005")},
				InstanceTypeID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
				SSHKeyGroupIDs:   &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
				OSProfile:        "ubuntu",
				ConfigProfile:    "default",
				NodeLabels:       &[]string{"label1", "label2"},
				DesiredNodeCount: 3,
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:          clusterID,
					OrgID:       orgID,
					SiteID:      siteID,
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
				}, nil)

				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(n *db.NodePool) bool {
					return n.Name == "test-pool-vpc" && n.NodeNamePrefix == "test-pool-vpc-prefix" && len(n.VpcPrefixIDs) > 0
				})).Return(db.NodePool{
					ID:             nodePoolID,
					Name:           "test-pool-vpc",
					NodeNamePrefix: "test-pool-vpc-prefix",
				}, nil)

				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionNodePool", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
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
				ID:          clusterID,
				OrgID:       orgID,
				SiteID:      siteID,
				PodCIDR:     "192.168.0.0/16",
				ServiceCIDR: "10.96.0.0/12",
			}, nil)

			// Determine if native networking should be enabled based on whether request uses VpcPrefixIDs
			hasVpcPrefixIDs := len(tt.request.VpcPrefixIDs) > 0

			// Mock GetSiteByID to return a site with appropriate native networking setting
			site := &forgesdk.Site{}
			site.SetId(siteID.String())
			capabilities := forgesdk.NewSiteCapabilities()
			capabilities.SetNativeNetworking(hasVpcPrefixIDs)
			site.SetCapabilities(*capabilities)

			mockForge := &mockForgeProvider{
				getSiteByIDFunc: func(ctx context.Context, orgID, siteID string) (*forgesdk.Site, error) {
					return site, nil
				},
			}

			if tt.setupMockCalls != nil {
				tt.setupMockCalls(mockDB.MockTx)
			}
			mockDB.MockTx.On(
				"ListNodePools",
				mock.Anything,
				db.NodePoolFilters{ClusterID: &clusterID},
			).Return([]db.NodePool{}, nil).Maybe()

			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, mockForge, config.TeleportConfig{}, false)

			resp, err := service.CreateNodePool(context.Background(), api.CreateNodePoolRequestObject{
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
				SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
				InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
				SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
				OSProfile:            "ubuntu",
				ConfigProfile:        "default",
				SystemReservedMemory: "16Gi",
				ControlPlane:         false,
				NodeLabels:           []string{"label1", "label2"},
				Conditions:           mockDBNodePoolConditions(now),
			},
			wantNodePool: api.GetNodePool200JSONResponse{
				ID:                   nodePoolID,
				ClusterID:            uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				Name:                 "test-pool",
				NodeNamePrefix:       "test-pool",
				SubnetIDs:            []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")},
				InstanceTypeID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
				SSHKeyGroupIDs:       []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
				OSProfile:            "ubuntu",
				ConfigProfile:        "default",
				SystemReservedMemory: func() *string { s := "16Gi"; return &s }(),
				ControlPlane:         false,
				NodeLabels:           []string{"label1", "label2"},
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

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, nil, config.TeleportConfig{}, false)

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
		name                  string
		nodePoolID            uuid.UUID
		body                  api.UpdateNodePoolRequest
		setupMockDB           func(*mockDB, *mockTx)
		forgeNativeNetworking bool
		expectError           bool        // if true, assert resp is nil and err != nil; else assert no error and validate response
		wantResponseType      interface{} // expected response type when no error (e.g. api.UpdateNodePool200JSONResponse{})
		verifyUpdatePool      func(t *testing.T, n *db.NodePool)
	}{
		{
			name:       "success with subnetIDs replace (differs) only updates pool; controller creates NodeSet on reconcile",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs: []openapi_types.UUID{subnetID2},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:               nodePoolID,
					ClusterID:        clusterID,
					SubnetIDs:        []uuid.UUID{subnetID1},
					CurrentNodeSetID: &currentNodeSetID,
					Conditions:       mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:               nodePoolID,
					SubnetIDs:        []uuid.UUID{subnetID2},
					CurrentNodeSetID: &currentNodeSetID,
				}, nil)
			},
			forgeNativeNetworking: false,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{subnetID2}, n.SubnetIDs)
				assert.Equal(t, currentNodeSetID, *n.CurrentNodeSetID)
			},
		},
		{
			name:       "success with subnetIDs replace same set (no new NodeSet)",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs: []openapi_types.UUID{subnetID1},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					SubnetIDs:  []uuid.UUID{subnetID1},
					Conditions: mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:        nodePoolID,
					SubnetIDs: []uuid.UUID{subnetID1},
				}, nil)
			},
			forgeNativeNetworking: false,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{subnetID1}, n.SubnetIDs)
			},
		},
		{
			name:       "empty subnetIDs replaces subnets with empty",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs:        []openapi_types.UUID{},
				DesiredNodeCount: testutil.PtrTo(3),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					SubnetIDs:  []uuid.UUID{subnetID1},
					Conditions: mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					SubnetIDs:    []uuid.UUID{},
					DesiredCount: 3,
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, []uuid.UUID{}, n.SubnetIDs)
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
					SubnetIDs:  []uuid.UUID{subnetID1, subnetID2},
					Conditions: mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					SubnetIDs:    []uuid.UUID{subnetID1, subnetID2},
					DesiredCount: 4,
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, []uuid.UUID{subnetID1, subnetID2}, n.SubnetIDs)
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
					SubnetIDs:  []uuid.UUID{subnetID1},
					Conditions: mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					SubnetIDs:    []uuid.UUID{subnetID1},
					DesiredCount: 5,
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{subnetID1}, n.SubnetIDs)
				assert.Equal(t, 5, n.DesiredCount)
			},
		},
		{
			name:       "subnetIDs with native networking returns 400",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs: []openapi_types.UUID{subnetID1},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
			},
			forgeNativeNetworking: true,
			wantResponseType:      api.UpdateNodePool400JSONResponse{},
		},
		{
			name:       "subnetIDs when node pool not found returns 404",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs: []openapi_types.UUID{subnetID1},
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
				SubnetIDs: []openapi_types.UUID{subnetID1},
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
				VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID2},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:               nodePoolID,
					ClusterID:        clusterID,
					VpcPrefixIDs:     []uuid.UUID{vpcPrefixID1},
					CurrentNodeSetID: &currentNodeSetID,
					Conditions:       mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:               nodePoolID,
					VpcPrefixIDs:     []uuid.UUID{vpcPrefixID2},
					CurrentNodeSetID: &currentNodeSetID,
				}, nil)
			},
			forgeNativeNetworking: true,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{vpcPrefixID2}, n.VpcPrefixIDs)
				assert.Equal(t, currentNodeSetID, *n.CurrentNodeSetID)
			},
		},
		{
			name:       "success with vpcPrefixIDs replace same set (no new NodeSet)",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:           nodePoolID,
					ClusterID:    clusterID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					Conditions:   mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
				}, nil)
			},
			forgeNativeNetworking: true,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, []uuid.UUID{vpcPrefixID1}, n.VpcPrefixIDs)
			},
		},
		{
			name:       "empty vpcPrefixIDs replaces vpc prefixes with empty",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				VpcPrefixIDs:     []openapi_types.UUID{},
				DesiredNodeCount: testutil.PtrTo(2),
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:           nodePoolID,
					ClusterID:    clusterID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					Conditions:   mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					VpcPrefixIDs: []uuid.UUID{},
					DesiredCount: 2,
				}, nil)
			},
			wantResponseType: api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, []uuid.UUID{}, n.VpcPrefixIDs)
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
					ID:           nodePoolID,
					ClusterID:    clusterID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1, vpcPrefixID2},
					Conditions:   mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1, vpcPrefixID2},
					DesiredCount: 6,
				}, nil)
			},
			forgeNativeNetworking: true,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, []uuid.UUID{vpcPrefixID1, vpcPrefixID2}, n.VpcPrefixIDs)
				assert.Equal(t, 6, n.DesiredCount)
			},
		},
		{
			name:       "subnetIDs replace when no current NodeSet only updates pool",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs: []openapi_types.UUID{subnetID2},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					SubnetIDs:  []uuid.UUID{subnetID1},
					Conditions: mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:        nodePoolID,
					SubnetIDs: []uuid.UUID{subnetID2},
				}, nil)
			},
			forgeNativeNetworking: false,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, []uuid.UUID{subnetID2}, n.SubnetIDs)
				assert.Nil(t, n.CurrentNodeSetID)
			},
		},
		{
			name:       "subnetIDs dedupe and same set no new NodeSet",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs: []openapi_types.UUID{subnetID1, subnetID1, subnetID2},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:         nodePoolID,
					ClusterID:  clusterID,
					SubnetIDs:  []uuid.UUID{subnetID2, subnetID1},
					Conditions: mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:        nodePoolID,
					SubnetIDs: []uuid.UUID{subnetID1, subnetID2},
				}, nil)
			},
			forgeNativeNetworking: false,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				require.Len(t, n.SubnetIDs, 2)
				assert.Contains(t, n.SubnetIDs, subnetID1)
				assert.Contains(t, n.SubnetIDs, subnetID2)
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
					ID:           nodePoolID,
					ClusterID:    clusterID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					Conditions:   mockDBNodePoolConditions(now),
				}, nil)
				tx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool")).Return(db.NodePool{
					ID:           nodePoolID,
					VpcPrefixIDs: []uuid.UUID{vpcPrefixID1},
					DesiredCount: 5,
				}, nil)
			},
			forgeNativeNetworking: true,
			wantResponseType:      api.UpdateNodePool200JSONResponse{},
			verifyUpdatePool: func(t *testing.T, n *db.NodePool) {
				t.Helper()
				assert.Equal(t, nodePoolID, n.ID)
				assert.Equal(t, []uuid.UUID{vpcPrefixID1}, n.VpcPrefixIDs)
				assert.Equal(t, 5, n.DesiredCount)
			},
		},
		{
			name:       "vpcPrefixIDs with non-native networking returns 400",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
			},
			forgeNativeNetworking: false,
			wantResponseType:      api.UpdateNodePool400JSONResponse{},
		},
		{
			name:       "both subnetIDs and vpcPrefixIDs returns 400",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				SubnetIDs:    []openapi_types.UUID{subnetID1},
				VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1},
			},
			setupMockDB: func(_ *mockDB, tx *mockTx) {
				tx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
				}, nil)
			},
			forgeNativeNetworking: false, // site capability irrelevant; validation rejects "both" first
			wantResponseType:      api.UpdateNodePool400JSONResponse{},
		},
		{
			name:       "vpcPrefixIDs when node pool not found returns 404",
			nodePoolID: nodePoolID,
			body: api.UpdateNodePoolRequest{
				VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1},
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
				VpcPrefixIDs: []openapi_types.UUID{vpcPrefixID1},
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
			}, nil).Once()
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Maybe().Return(db.Cluster{
				ID:          clusterID,
				OrgID:       testOrgID,
				SiteID:      siteID,
				PodCIDR:     "192.168.0.0/16",
				ServiceCIDR: "10.96.0.0/12",
			}, nil)
			mockDB.MockTx.On(
				"ListNodePools",
				mock.Anything,
				db.NodePoolFilters{ClusterID: &clusterID},
			).Maybe().Return([]db.NodePool{{ID: nodePoolID}}, nil)
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Maybe().Return(nil)

			site := &forgesdk.Site{}
			site.SetId(siteID.String())
			capabilities := forgesdk.NewSiteCapabilities()
			capabilities.SetNativeNetworking(tt.forgeNativeNetworking)
			site.SetCapabilities(*capabilities)
			mockForge := &mockForgeProvider{
				getSiteByIDFunc: func(ctx context.Context, orgID, siteID string) (*forgesdk.Site, error) {
					return site, nil
				},
			}

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, mockForge, config.TeleportConfig{}, false)

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
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:          clusterID,
		SiteID:      uuid.New(),
		PodCIDR:     "10.240.0.0/23",
		ServiceCIDR: "10.120.0.0/24",
	}, nil)
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

	service := server.NewNodePoolService(&slog.Logger{}, mockDB, nil, config.TeleportConfig{}, false)
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

			service := server.NewNodePoolService(&slog.Logger{}, mockDB, nil, config.TeleportConfig{}, false)

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
