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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// testOrgID is a real orgID from the tenant list for use in tests
const testOrgID = "novxwn552ssc"

type clusterService interface {
	ListClusters(ctx context.Context, req api.ListClustersRequestObject) (api.ListClustersResponseObject, error)
	CreateCluster(ctx context.Context, req api.CreateClusterRequestObject) (api.CreateClusterResponseObject, error)
	GetCluster(ctx context.Context, req api.GetClusterRequestObject) (api.GetClusterResponseObject, error)
	UpdateCluster(ctx context.Context, req api.UpdateClusterRequestObject) (api.UpdateClusterResponseObject, error)
	DeleteCluster(ctx context.Context, req api.DeleteClusterRequestObject) (api.DeleteClusterResponseObject, error)
}

func newTestClusterService(logger *slog.Logger, mockDB *mockDB) clusterService {
	return server.NewClusterService(logger, mockDB, server.NewNodePoolService(logger, mockDB, config.TeleportConfig{}, nil), config.TeleportConfig{}, false, nil, true, "")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// forgeDBCluster stamps the provider type that a stored row always carries. The
// API mappers key off it, so a fixture built as a bare struct literal would
// surface no provider at all.
func forgeDBCluster(cluster db.Cluster) db.Cluster {
	// Read the payload directly: NicoProvider gates on the provider type, which is
	// exactly what these fixtures have not set yet.
	var nico db.NicoClusterProvider
	if cluster.Nico != nil {
		nico = *cluster.Nico
	}
	cluster.SetNicoProvider(nico)
	return cluster
}

// nicoClusterProvider is the API-shaped Forge identity of the clusters built by
// forgeDBCluster.
func nicoClusterProvider(siteID, vpcID uuid.UUID) *api.NicoClusterProvider {
	return &api.NicoClusterProvider{OrgID: testOrgID, SiteID: siteID, VpcID: vpcID}
}

func TestClusterService_ListClusters(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		nameFilter  *string
		dbClusters  []db.Cluster
		dbError     error
		wantError   bool
		wantCluster []api.Cluster
	}{
		{
			name: "success",
			dbClusters: []db.Cluster{
				forgeDBCluster(db.Cluster{
					ID:                uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					Name:              "test-cluster",
					CniPlugin:         "calico",
					KubernetesVersion: "1.29.0",
					ControlPlaneVIP:   "10.0.0.1",
					PodCIDR:           "192.168.0.0/16",
					ServiceCIDR:       "10.96.0.0/12",
					Conditions:        mockDBClusterConditions(now),
					Nico: &db.NicoClusterProvider{
						OrgID:  testOrgID,
						SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
						VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
					},
				}),
			},
			wantCluster: []api.Cluster{
				{
					ID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					Name: "test-cluster",
					Nico: nicoClusterProvider(
						uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
						uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
					),
					CniPlugin:                     "calico",
					CniVersion:                    db.DefaultCalicoCNIVersion,
					ControlPlaneKubernetesVersion: "1.29.0",
					ControlPlaneVIP:               func() *string { s := "10.0.0.1"; return &s }(),
					EnableCPNodeRecycling:         func() *bool { b := false; return &b }(),
					TeleportEnabled:               func() *bool { b := false; return &b }(),
					ControlPlaneConfig:            defaultAPIControlPlaneConfig(),
					Network: api.ClusterNetwork{
						PodCIDR:     "192.168.0.0/16",
						ServiceCIDR: "10.96.0.0/12",
					},
					Conditions: mockAPICClusterConditions(now),
				},
			},
		},
		{
			name:       "success with name filter",
			nameFilter: func() *string { s := "test-cluster"; return &s }(),
			dbClusters: []db.Cluster{
				forgeDBCluster(db.Cluster{
					ID:                uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					Name:              "test-cluster",
					CniPlugin:         "calico",
					KubernetesVersion: "1.29.0",
					ControlPlaneVIP:   "10.0.0.1",
					PodCIDR:           "192.168.0.0/16",
					ServiceCIDR:       "10.96.0.0/12",
					Conditions:        mockDBClusterConditions(now),
					Nico: &db.NicoClusterProvider{
						OrgID:  testOrgID,
						SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
						VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
					},
				}),
			},
			wantCluster: []api.Cluster{
				{
					ID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					Name: "test-cluster",
					Nico: nicoClusterProvider(
						uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
						uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
					),
					CniPlugin:                     "calico",
					CniVersion:                    db.DefaultCalicoCNIVersion,
					ControlPlaneKubernetesVersion: "1.29.0",
					ControlPlaneVIP:               func() *string { s := "10.0.0.1"; return &s }(),
					EnableCPNodeRecycling:         func() *bool { b := false; return &b }(),
					TeleportEnabled:               func() *bool { b := false; return &b }(),
					ControlPlaneConfig:            defaultAPIControlPlaneConfig(),
					Network: api.ClusterNetwork{
						PodCIDR:     "192.168.0.0/16",
						ServiceCIDR: "10.96.0.0/12",
					},
					Conditions: mockAPICClusterConditions(now),
				},
			},
		},
		{
			name:      "db error",
			dbError:   errors.New("db error"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.On("ListClusters", mock.Anything, mock.MatchedBy(func(filters db.ClusterFilters) bool {
				return (filters.Name == nil && tt.nameFilter == nil) ||
					(filters.Name != nil && tt.nameFilter != nil && *filters.Name == *tt.nameFilter)
			})).Return(tt.dbClusters, tt.dbError)

			service := newTestClusterService(discardLogger(), mockDB)

			req := api.ListClustersRequestObject{}
			if tt.nameFilter != nil {
				req.Params.Name = tt.nameFilter
			}
			resp, err := service.ListClusters(context.Background(), req)

			if tt.wantError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			response := resp.(api.ListClusters200JSONResponse)
			assert.Equal(t, tt.wantCluster, response.Clusters)
			mockDB.AssertExpectations(t)
		})
	}
}

func nicoCreateControlPlaneConfig(desiredCount int, vpcPrefixes bool) *api.CreateClusterControlPlaneConfig {
	nico := &api.NicoControlPlaneProviderCreate{
		InstanceTypePreference: []uuid.UUID{uuid.New()},
		Release:                "ubuntu",
		ConfigProfile:          "default",
	}
	if vpcPrefixes {
		nico.VpcPrefixIDs = []uuid.UUID{uuid.New()}
	} else {
		nico.SubnetIDs = []uuid.UUID{uuid.New()}
	}
	return &api.CreateClusterControlPlaneConfig{DesiredCount: desiredCount, Nico: nico}
}

func expectControlPlaneConfigCreate(tx *mockTx, clusterID uuid.UUID) {
	cfgID := uuid.New()
	tx.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
		return row.ClusterID == clusterID && row.Config.Nico != nil
	})).Return(db.ControlPlaneConfiguration{ID: cfgID, ClusterID: clusterID}, nil)
	tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID && c.ControlPlaneConfig.ConfigurationID == cfgID
	})).Return(db.Cluster{ID: clusterID}, nil)
}

func TestClusterService_CreateCluster(t *testing.T) {
	siteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	// Use a real orgID from the tenant list so findTenantByOrgID works
	orgID := testOrgID
	tests := []struct {
		name                  string
		request               api.CreateClusterRequest
		setupMockCalls        func(*mockTx)
		wantResponseError     bool
		responseErrorContains string
		responseStatus        int
		wantError             bool
		errorContains         string
	}{
		{
			name: "success",
			request: api.CreateClusterRequest{
				Name:                          "test-cluster",
				Nico:                          nicoClusterProvider(siteID, uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "ovn-kubernetes",
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneVIP:               "172.31.1.10",
				ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
				Network: &api.ClusterNetworkCreateConfig{
					PodCIDR:     "10.233.65.7/18",
					ServiceCIDR: "10.233.1.5/18",
				},
				NodePools: &[]api.NodePoolCreateConfig{
					{
						Name:           "w",
						NodeNamePrefix: "w-prefix",
						Nico: &api.NicoNodePoolProvider{
							SubnetIDs:      []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440003")},
							InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440004"),
							SSHKeyGroupIDs: &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
							Release:        "ubuntu",
							ConfigProfile:  "default",
						},
						NodeLabels:         map[string]string{"label1": "value1", "label2": "value2"},
						MachineAnnotations: map[string]string{"example.com/profiles": "gpu,network"},
						MachineLabels:      map[string]string{"example.com/pool": "gpu"},
						NodeTaints:         []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
						DesiredNodeCount:   1,
					},
				},
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("CreateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
					nico, err := c.NicoProvider()
					return err == nil &&
						c.Name == "test-cluster" &&
						nico.OrgID == orgID &&
						c.PodCIDR == "10.233.64.0/18" &&
						c.ServiceCIDR == "10.233.0.0/18" &&
						c.ControlPlaneVIP == "172.31.1.10"
				})).Return(forgeDBCluster(db.Cluster{
					ID:              clusterID,
					Name:            "test-cluster",
					PodCIDR:         "10.233.64.0/18",
					ServiceCIDR:     "10.233.0.0/18",
					ControlPlaneVIP: "172.31.1.10",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}), nil)

				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionCluster", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
					m, ok := input.(map[string]interface{})
					return ok && m["clusterID"] == clusterID
				})).Return(db.WorkflowOutbox{}, nil)

				workerNodePoolID := uuid.New()
				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(np *db.NodePool) bool {
					return np.ClusterID == clusterID &&
						!np.ControlPlane &&
						np.NodeNamePrefix == "w-prefix" &&
						assert.ObjectsAreEqual(map[string]string{"label1": "value1", "label2": "value2"}, np.NodeLabels) &&
						assert.ObjectsAreEqual(map[string]string{"example.com/profiles": "gpu,network"}, np.MachineAnnotations) &&
						assert.ObjectsAreEqual(map[string]string{"example.com/pool": "gpu"}, np.MachineLabels) &&
						assert.ObjectsAreEqual([]db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}}, np.NodeTaints)
				})).Return(db.NodePool{
					ID:             workerNodePoolID,
					ControlPlane:   false,
					NodeNamePrefix: "w-prefix",
				}, nil)

				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionNodePool", mock.Anything, mock.Anything).
					Return(db.WorkflowOutbox{}, nil)

				expectControlPlaneConfigCreate(tx, clusterID)
			},
			responseStatus: http.StatusCreated,
		},
		{
			name: "success with VPC prefixes (native networking enabled)",
			request: api.CreateClusterRequest{
				Name:                          "test-cluster-vpc",
				Nico:                          nicoClusterProvider(siteID, uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "calico",
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, true),
				NodePools: &[]api.NodePoolCreateConfig{
					{
						Name:           "w",
						NodeNamePrefix: "w-prefix",
						Nico: &api.NicoNodePoolProvider{
							VpcPrefixIDs:   []uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440005")},
							InstanceTypeID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440004"),
							SSHKeyGroupIDs: &[]uuid.UUID{uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")},
							Release:        "ubuntu",
							ConfigProfile:  "default",
						},
						NodeLabels:       map[string]string{"label1": "value1", "label2": "value2"},
						NodeTaints:       []api.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: api.NodeTaintEffectNoSchedule}},
						DesiredNodeCount: 1,
					},
				},
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("CreateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
					nico, err := c.NicoProvider()
					return err == nil &&
						c.Name == "test-cluster-vpc" &&
						nico.OrgID == orgID &&
						c.PodCIDR == "192.168.0.0/16" &&
						c.ServiceCIDR == "10.96.0.0/12" &&
						c.ControlPlaneVIP == ""
				})).Return(forgeDBCluster(db.Cluster{
					ID:          clusterID,
					Name:        "test-cluster-vpc",
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
					Nico: &db.NicoClusterProvider{
						OrgID:  orgID,
						SiteID: siteID,
					},
				}), nil)

				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionCluster", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
					m, ok := input.(map[string]interface{})
					return ok && m["clusterID"] == clusterID
				})).Return(db.WorkflowOutbox{}, nil)

				workerNodePoolID := uuid.New()
				tx.On("CreateNodePool", mock.Anything, mock.MatchedBy(func(np *db.NodePool) bool {
					nico, err := np.NicoProvider()
					return err == nil &&
						np.ClusterID == clusterID &&
						!np.ControlPlane &&
						np.NodeNamePrefix == "w-prefix" &&
						len(nico.VpcPrefixIDs) > 0 &&
						assert.ObjectsAreEqual(map[string]string{"label1": "value1", "label2": "value2"}, np.NodeLabels) &&
						assert.ObjectsAreEqual([]db.NodeTaint{{Key: "nvidia.com/dedicated", Value: "gpu", Effect: "NoSchedule"}}, np.NodeTaints)
				})).Return(db.NodePool{
					ID:             workerNodePoolID,
					ControlPlane:   false,
					NodeNamePrefix: "w-prefix",
				}, nil)

				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionNodePool", mock.Anything, mock.Anything).
					Return(db.WorkflowOutbox{}, nil)

				expectControlPlaneConfigCreate(tx, clusterID)
			},
			responseStatus: http.StatusCreated,
		},
		{
			name: "duplicate cluster name",
			request: api.CreateClusterRequest{
				Name:                          "test-cluster",
				Nico:                          nicoClusterProvider(uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"), uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "calico",
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("CreateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
					nico, err := c.NicoProvider()
					return err == nil && c.Name == "test-cluster" && nico.OrgID == orgID
				})).Return(db.Cluster{}, db.ErrClusterNameNotUnique)
			},
			wantResponseError: true,
			responseStatus:    http.StatusBadRequest,
			// response is json so the quotes are escaped
			responseErrorContains: "A cluster already exists with the name \\\"test-cluster\\\"",
		},
		{
			name: "missing controlPlaneConfig",
			request: api.CreateClusterRequest{
				Name:                          "test-cluster",
				Nico:                          nicoClusterProvider(uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"), uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "calico",
				ControlPlaneKubernetesVersion: "1.29.0",
				NodePools:                     &[]api.NodePoolCreateConfig{},
			},
			wantResponseError:     true,
			responseStatus:        http.StatusBadRequest,
			responseErrorContains: "controlPlaneConfig is required",
		},
		{
			name: "cluster creation error",
			request: api.CreateClusterRequest{
				Name:                          "test-cluster",
				Nico:                          nicoClusterProvider(uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"), uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "calico",
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("CreateCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, errors.New("create cluster error"))
			},
			wantError: true,
		},
		{
			name: "control-plane node pool rejected",
			request: api.CreateClusterRequest{
				Name:                          "test-cluster",
				Nico:                          nicoClusterProvider(siteID, uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "calico",
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
				NodePools: &[]api.NodePoolCreateConfig{
					{
						DesiredNodeCount: 1,
						ControlPlane:     true,
					},
				},
			},
			wantResponseError:     true,
			responseStatus:        http.StatusBadRequest,
			responseErrorContains: "control-plane node pools are not allowed",
		},
		{
			name: "cluster name too long for cluster namespace",
			request: api.CreateClusterRequest{
				Name:                          strings.Repeat("a", capi.ClusterNameMaxLength+1),
				Nico:                          nicoClusterProvider(siteID, uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")),
				CniPlugin:                     "calico",
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
			},
			wantResponseError:     true,
			responseStatus:        http.StatusBadRequest,
			responseErrorContains: "cluster name must be 55 characters or fewer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}

			// Mock GetCluster on mockDB for validation (validation uses s.db, not the transaction)
			// Only mock if the request has node pools that would trigger validation
			var service clusterService

			var requestNodePools []api.NodePoolCreateConfig
			if tt.request.NodePools != nil {
				requestNodePools = *tt.request.NodePools
			}

			if len(requestNodePools) > 0 {
				// Validation is called inside createInitialNodePools, which happens after the cluster is created
				// So we need to mock GetCluster with the clusterID that will be returned from CreateCluster
				// But validation happens before CreateCluster completes, so we need to mock it with the expected clusterID
				// Actually, validation uses the cluster.ID from the created cluster, so we need to mock it with clusterID
				mockDB.On("GetCluster", mock.Anything, clusterID).Return(forgeDBCluster(db.Cluster{
					ID: clusterID,
					Nico: &db.NicoClusterProvider{
						OrgID:  tt.request.Nico.OrgID,
						SiteID: tt.request.Nico.SiteID,
					},
				}), nil).Maybe() // Use Maybe() since validation might not be called if CreateCluster fails early

				service = newTestClusterService(discardLogger(), mockDB)
			} else {
				service = newTestClusterService(discardLogger(), mockDB)
			}

			if tt.setupMockCalls != nil {
				tt.setupMockCalls(mockDB.MockTx)
				mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			}

			resp, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{
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
			require.NoError(t, resp.VisitCreateClusterResponse(responseWriter))
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

func TestClusterService_CreateClusterRejectsInsufficientPodCIDRCapacity(t *testing.T) {
	mockDB := &mockDB{}
	service := newTestClusterService(discardLogger(), mockDB)
	resp, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		Body: &api.CreateClusterRequest{
			Name:                          "test-cluster",
			Nico:                          nicoClusterProvider(uuid.New(), uuid.New()),
			CniPlugin:                     "ovn-kubernetes",
			ControlPlaneKubernetesVersion: "1.29.0",
			ControlPlaneVIP:               "192.168.1.10",
			Network: &api.ClusterNetworkCreateConfig{
				PodCIDR:     "10.240.0.0/23",
				ServiceCIDR: "10.120.0.0/24",
			},
			ControlPlaneConfig: nicoCreateControlPlaneConfig(2, false),
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.CreateCluster400JSONResponse)
	require.True(t, ok, "expected CreateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "provides 2 /24 node blocks, but 3 are required")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_CreateClusterRejectsControlPlaneVIPOverlap(t *testing.T) {
	siteID := uuid.New()
	mockDB := &mockDB{MockTx: &mockTx{}}

	service := newTestClusterService(discardLogger(), mockDB)
	resp, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		Body: &api.CreateClusterRequest{
			Name:                          "test-cluster",
			Nico:                          nicoClusterProvider(siteID, uuid.New()),
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			ControlPlaneVIP:               "192.168.1.10",
			ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.CreateCluster400JSONResponse)
	require.True(t, ok, "expected CreateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "controlPlaneVIP")
	assert.Contains(t, badRequest.Message, "network.podCIDR")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_CreateClusterRejectsCustomNetworkForCalico(t *testing.T) {
	mockDB := &mockDB{MockTx: &mockTx{}}
	service := newTestClusterService(discardLogger(), mockDB)

	resp, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		Body: &api.CreateClusterRequest{
			Name:                          "test-cluster",
			Nico:                          nicoClusterProvider(uuid.New(), uuid.New()),
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			Network: &api.ClusterNetworkCreateConfig{
				PodCIDR:     "10.240.0.0/16",
				ServiceCIDR: "10.120.0.0/16",
			},
			ControlPlaneConfig: nicoCreateControlPlaneConfig(1, false),
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.CreateCluster400JSONResponse)
	require.True(t, ok, "expected CreateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "custom pod or service CIDRs require CNI plugin \"ovn-kubernetes\"")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_CreateClusterRejectsUnsupportedInfraProvider(t *testing.T) {
	mockDB := &mockDB{MockTx: &mockTx{}}
	service := server.NewClusterService(
		discardLogger(),
		mockDB,
		nil,
		config.TeleportConfig{},
		false,
		[]string{config.InfraProviderNico},
		false,
		"",
	)

	resp, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		Body: &api.CreateClusterRequest{
			Name:                          "test-cluster",
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			Infra: &api.InfraClusterProvider{
				Cluster: map[string]interface{}{
					"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
					"kind":       "DockerCluster",
				},
			},
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.CreateCluster400JSONResponse)
	require.True(t, ok, "expected CreateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "infra provider is not supported by this deployment")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_GetCluster(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	now := time.Now()
	tests := []struct {
		name        string
		clusterID   uuid.UUID
		dbCluster   db.Cluster
		dbError     error
		wantError   bool
		want404     bool
		wantCluster api.GetCluster200JSONResponse
	}{
		{
			name:      "success",
			clusterID: clusterID,
			dbCluster: forgeDBCluster(db.Cluster{
				ID:                clusterID,
				Name:              "test-cluster",
				CniPlugin:         "calico",
				KubernetesVersion: "1.29.0",
				ControlPlaneVIP:   "10.0.0.1",
				PodCIDR:           "192.168.0.0/16",
				ServiceCIDR:       "10.96.0.0/12",
				Conditions:        mockDBClusterConditions(now),
				Nico: &db.NicoClusterProvider{
					OrgID:  testOrgID,
					SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
					VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
				},
			}),
			wantCluster: api.GetCluster200JSONResponse{
				ID:   clusterID,
				Name: "test-cluster",
				Nico: nicoClusterProvider(
					uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
					uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
				),
				CniPlugin:                     "calico",
				CniVersion:                    db.DefaultCalicoCNIVersion,
				ControlPlaneKubernetesVersion: "1.29.0",
				ControlPlaneVIP:               func() *string { s := "10.0.0.1"; return &s }(),
				EnableCPNodeRecycling:         func() *bool { b := false; return &b }(),
				TeleportEnabled:               func() *bool { b := false; return &b }(),
				ControlPlaneConfig:            defaultAPIControlPlaneConfig(),
				Network: api.ClusterNetwork{
					PodCIDR:     "192.168.0.0/16",
					ServiceCIDR: "10.96.0.0/12",
				},
				Conditions: mockAPICClusterConditions(now),
			},
		},
		{
			name:      "not found",
			clusterID: clusterID,
			dbError:   db.ErrNotFound,
			want404:   true,
		},
		{
			name:      "db error",
			clusterID: clusterID,
			dbError:   errors.New("db error"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.On("GetCluster", mock.Anything, tt.clusterID).Return(tt.dbCluster, tt.dbError)

			service := newTestClusterService(discardLogger(), mockDB)

			resp, err := service.GetCluster(context.Background(), api.GetClusterRequestObject{
				ClusterID: tt.clusterID,
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}
			if tt.want404 {
				assert.IsType(t, api.GetCluster404Response{}, resp)
				return
			}

			require.NoError(t, err)
			response, ok := resp.(api.GetCluster200JSONResponse)
			require.True(t, ok)
			assert.Equal(t, tt.wantCluster, response)
			mockDB.AssertExpectations(t)
		})
	}
}

func TestClusterService_DeleteCluster(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	updateFailed := db.NewCondition(false, "Cluster update failed")
	tests := []struct {
		name             string
		clusterID        uuid.UUID
		dbCluster        db.Cluster
		getClusterErr    error
		updateErr        error
		workflowErr      error
		txCommitErr      error
		wantError        bool
		want404          bool
		setupMockCalls   func(*mockTx)
		verifyConditions func(t *testing.T, conditions db.ClusterConditions)
	}{
		{
			name:      "success",
			clusterID: clusterID,
			dbCluster: db.Cluster{
				ID:                clusterID,
				Name:              "test-cluster",
				CniPlugin:         "calico",
				KubernetesVersion: "1.29.0",
				ControlPlaneVIP:   "10.0.0.1",
				Nico: &db.NicoClusterProvider{
					OrgID:  testOrgID,
					SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
					VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
				},
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:         clusterID,
					Conditions: db.ClusterConditions{UpdateFailed: &updateFailed},
				}, nil)
				tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
					return c.ID == clusterID && c.Conditions.Deleting.Status == db.ConditionStatusTrue
				})).Return(db.Cluster{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, "TeardownCluster", mock.Anything, mock.MatchedBy(func(input interface{}) bool {
					m, ok := input.(map[string]interface{})
					return ok && m["clusterID"] == clusterID
				})).Return(db.WorkflowOutbox{}, nil)
			},
			verifyConditions: func(t *testing.T, conditions db.ClusterConditions) {
				t.Helper()
				assert.Equal(t, db.ConditionStatusTrue, conditions.Deleting.Status)
				require.NotNil(t, conditions.UpdateFailed)
				assert.True(t, conditions.UpdateFailed.Equal(updateFailed))
			},
		},
		{
			name:          "not found",
			clusterID:     clusterID,
			getClusterErr: db.ErrNotFound,
			want404:       true,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)
			},
		},
		{
			name:      "update cluster error",
			clusterID: clusterID,
			updateErr: errors.New("update error"),
			wantError: true,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, errors.New("update error"))
			},
		},
		{
			name:        "workflow entry error",
			clusterID:   clusterID,
			workflowErr: errors.New("workflow error"),
			wantError:   true,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(db.WorkflowOutbox{}, errors.New("workflow error"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}

			if tt.setupMockCalls != nil {
				tt.setupMockCalls(mockDB.MockTx)
			}

			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(tt.txCommitErr)

			service := newTestClusterService(discardLogger(), mockDB)

			resp, err := service.DeleteCluster(context.Background(), api.DeleteClusterRequestObject{
				ClusterID: tt.clusterID,
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}
			if tt.want404 {
				assert.IsType(t, api.DeleteCluster404Response{}, resp)
				return
			}

			require.NoError(t, err)
			require.IsType(t, api.DeleteCluster202Response{}, resp)

			// Verify conditions if a verification function was provided
			if tt.verifyConditions != nil {
				var conditions db.ClusterConditions
				for _, call := range mockDB.MockTx.Calls {
					if call.Method == "UpdateCluster" {
						cluster := call.Arguments[1].(*db.Cluster)
						conditions = cluster.Conditions
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

// defaultAPIControlPlaneConfig is what a cluster whose control plane config has
// never been written reports. The Forge fields live under nico, and that payload
// is absent until something writes it, so no provider object is emitted.
func defaultAPIControlPlaneConfig() *api.ControlPlaneConfig {
	maxSurge := 0
	autoScaleDownDisabled := false
	provisioningMonitorDisabled := false
	teleportEnabled := false
	return &api.ControlPlaneConfig{
		MaxSurge:                    &maxSurge,
		AutoScaleDownDisabled:       &autoScaleDownDisabled,
		ProvisioningMonitorDisabled: &provisioningMonitorDisabled,
		TeleportEnabled:             &teleportEnabled,
	}
}

func mockDBClusterConditions(now time.Time) db.ClusterConditions {
	return db.ClusterConditions{
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
		ControlPlaneUpgrading: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "No upgrade in progress",
			LastTransitionTime: now,
		},
		CPNodesRecycling: db.Condition{
			Status:             db.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: time.Time{},
		},
	}
}

func mockAPICClusterConditions(now time.Time) api.ClusterConditions {
	return api.ClusterConditions{
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
		ControlPlaneUpgrading: &api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "No upgrade in progress",
			LastTransitionTime: now,
		},
		CpNodesRecycling: &api.Condition{
			Status:             api.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: time.Time{},
		},
	}
}

func TestValidateKubernetesVersionSkewWithControlPlane(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		newVersion     string
		expectError    bool
		errorMsg       string
	}{
		{
			name:           "valid upgrade - same minor, patch bump",
			currentVersion: "1.30.10",
			newVersion:     "1.30.14",
			expectError:    false,
		},
		{
			name:           "valid upgrade - one minor version",
			currentVersion: "1.29.10",
			newVersion:     "1.30.14",
			expectError:    false,
		},
		{
			name:           "invalid - same version",
			currentVersion: "1.30.14",
			newVersion:     "1.30.14",
			expectError:    true,
			errorMsg:       "must be greater than the current version",
		},
		{
			name:           "invalid - major version downgrade",
			currentVersion: "2.0.0",
			newVersion:     "1.30.14",
			expectError:    true,
			errorMsg:       "kubernetes version downgrade not supported",
		},
		{
			name:           "invalid - minor version downgrade",
			currentVersion: "1.30.10",
			newVersion:     "1.29.14",
			expectError:    true,
			errorMsg:       "kubernetes version downgrade not supported",
		},
		{
			name:           "invalid - patch version downgrade",
			currentVersion: "1.30.14",
			newVersion:     "1.30.10",
			expectError:    true,
			errorMsg:       "kubernetes version downgrade not supported",
		},
		{
			name:           "invalid - skip minor version",
			currentVersion: "1.28.10",
			newVersion:     "1.30.14",
			expectError:    true,
			errorMsg:       "control plane can only be upgraded 1 minor version at a time",
		},
		{
			name:           "invalid - skip multiple minor versions",
			currentVersion: "1.27.10",
			newVersion:     "1.30.14",
			expectError:    true,
			errorMsg:       "control plane can only be upgraded 1 minor version at a time",
		},
		{
			name:           "valid - major version upgrade",
			currentVersion: "1.30.10",
			newVersion:     "2.0.0",
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := server.ValidateKubernetesVersionSkewWithControlPlane(tt.currentVersion, tt.newVersion)
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateKubernetesVersionSkewWithWorkerNodes(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")

	tests := []struct {
		name         string
		workerPools  []db.NodePool
		newCPVersion string
		expectError  bool
		errorMsg     string
	}{
		{
			name:         "no worker pools - success",
			workerPools:  nil,
			newCPVersion: "1.32.0",
			expectError:  false,
		},
		{
			name: "worker at 1.30, CP 1.32 - within skew (N-3)",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: "1.30.10"},
			},
			newCPVersion: "1.32.0",
			expectError:  false,
		},
		{
			name: "worker at 1.29, CP 1.32 - within skew (3 minor behind, N-3)",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: "1.29.10"},
			},
			newCPVersion: "1.32.0",
			expectError:  false,
		},
		{
			name: "worker at 1.31, CP 1.32 - within skew",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: "1.31.5"},
			},
			newCPVersion: "1.32.0",
			expectError:  false,
		},
		{
			name: "worker at 1.28, CP 1.32 - exceeds skew (4 minor behind, N-3)",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: "1.28.10"},
			},
			newCPVersion: "1.32.0",
			expectError:  true,
			errorMsg:     "more than 3 minor versions behind",
		},
		{
			name: "worker at 1.26, CP 1.30 - exceeds skew (4 minor behind)",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: "1.26.5"},
			},
			newCPVersion: "1.30.0",
			expectError:  true,
			errorMsg:     "more than 3 minor versions behind",
		},
		{
			name: "worker major 1, CP major 2 - different major rejected",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: "1.30.0"},
			},
			newCPVersion: "2.0.0",
			expectError:  true,
			errorMsg:     "does not match requested control plane major version",
		},
		{
			name: "worker empty kubernetes_version - skipped",
			workerPools: []db.NodePool{
				{Name: "workers", KubernetesVersion: ""},
			},
			newCPVersion: "1.32.0",
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockTx := &testutil.MockTx{}
			controlPlaneFalse := false
			mockTx.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
				return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane == controlPlaneFalse
			})).Return(tt.workerPools, nil)

			err := server.ValidateKubernetesVersionSkewWithWorkerNodes(context.Background(), mockTx, clusterID, tt.newCPVersion)
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
			mockTx.AssertExpectations(t)
		})
	}
}

// TestClusterService_UpdateCluster_Rejects409WhenUpdateInProgress covers the
// mutual-exclusion guard: when UpdateInProgress is set on the cluster row,
// any PATCH mutation must be rejected with 409 Conflict. The handler loads
// the cluster via GetClusterForUpdate (SELECT FOR UPDATE) so the conflict
// check is atomic with the row lock.
func TestClusterService_UpdateCluster_Rejects409WhenUpdateInProgress(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	started := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}

	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{
				Type:      db.OperationTypeK8sUpgrade,
				Message:   "k8s upgrade in flight",
				StartedAt: started,
			},
		},
	}, nil).Once()

	// The MockDB's RunInTx executes the callback first and returns the callback's
	// error if non-nil (see testutil.MockDB.RunInTx). The `Return(nil)` here is
	// only the mock's own fallthrough value and is never reached — the callback
	// short-circuits with *updateInProgressError, which the handler maps to 409.
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{},
	})
	require.NoError(t, err)

	conflict, ok := resp.(api.UpdateCluster409JSONResponse)
	require.True(t, ok, "expected UpdateCluster409JSONResponse, got %T", resp)
	assert.Contains(t, conflict.Message, "k8s_upgrade")
	mockDB.MockTx.AssertExpectations(t)
}

func TestClusterService_UpdateCluster_EnableEtcdEncryption_StartsWorkflow(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	enable := true
	startedAfter := time.Now().Add(-1 * time.Second)

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}

	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		Name:          "test-cluster",
		EtcdEncrypted: false,
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:    db.Condition{Status: db.ConditionStatusFalse},
		},
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()
	mockDB.MockTx.On(
		"CreateWorkflowOutboxEntry",
		mock.Anything,
		constants.EnableEtcdEncryptionWorkflowType,
		constants.EnableEtcdEncryptionWorkflowPrefix+clusterID.String(),
		mock.MatchedBy(func(input interface{}) bool {
			m, ok := input.(map[string]interface{})
			_, hasNodePoolID := m["nodePoolID"]
			return ok && m["clusterID"] == clusterID && !hasNodePoolID
		}),
	).Return(db.WorkflowOutbox{}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		uip := c.Conditions.UpdateInProgress
		return c.ID == clusterID &&
			!c.EtcdEncrypted &&
			uip != nil &&
			uip.Type == db.OperationTypeEtcdEncryption &&
			uip.Message == "enabling etcd encryption" &&
			!strings.Contains(uip.Message, clusterID.String()) &&
			uip.StartedAt.After(startedAfter)
	})).Return(db.Cluster{
		ID:            clusterID,
		Name:          "test-cluster",
		EtcdEncrypted: false,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption, StartedAt: time.Now()},
		},
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableEtcdEncryption: &enable},
	})
	require.NoError(t, err)

	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	require.NotNil(t, body.Conditions.UpdateInProgress)
	assert.Equal(t, api.UpdateInProgressTypeEtcdEncryption, body.Conditions.UpdateInProgress.Type)
	mockDB.MockTx.AssertExpectations(t)
}

func TestClusterService_UpdateCluster_EnableEtcdEncryption_RejectsFalse(t *testing.T) {
	enable := false

	mockDB := &mockDB{}
	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: uuid.New(),
		Body:      &api.UpdateClusterRequest{EnableEtcdEncryption: &enable},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "disabling etcd encryption is not supported")
}

func TestClusterService_UpdateCluster_RejectsNetworkChange(t *testing.T) {
	mockDB := &mockDB{}
	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: uuid.New(),
		Body: &api.UpdateClusterRequest{
			Network: &api.ClusterNetworkCreateConfig{PodCIDR: "10.233.64.0/18"},
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "network is immutable")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableEtcdEncryption_AlreadyEncryptedNoop(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	enable := true

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		Name:          "test-cluster",
		EtcdEncrypted: true,
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableEtcdEncryption: &enable},
	})
	require.NoError(t, err)

	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	assert.True(t, body.EtcdEncrypted)
	assert.Nil(t, body.Conditions.UpdateInProgress)
	mockDB.MockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableEtcdEncryption_RejectsLegacyOperation(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	started := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	enable := true

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		EtcdEncrypted: false,
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
			ControlPlaneUpgrading: db.Condition{
				Status:             db.ConditionStatusTrue,
				Message:            "upgrade in flight",
				LastTransitionTime: started,
			},
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableEtcdEncryption: &enable},
	})
	require.NoError(t, err)

	conflict, ok := resp.(api.UpdateCluster409JSONResponse)
	require.True(t, ok, "expected UpdateCluster409JSONResponse, got %T", resp)
	assert.Contains(t, conflict.Message, "k8s_upgrade")
	mockDB.MockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableOIDC_StartsWorkflow(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	enable := true
	startedAfter := time.Now().Add(-1 * time.Second)

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:              clusterID,
		Name:            "test-cluster",
		OIDCAuthEnabled: false,
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:    db.Condition{Status: db.ConditionStatusFalse},
		},
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()
	mockDB.MockTx.On(
		"CreateWorkflowOutboxEntry",
		mock.Anything,
		constants.EnableOIDCWorkflowType,
		constants.EnableOIDCWorkflowPrefix+clusterID.String(),
		mock.MatchedBy(func(input interface{}) bool {
			m, ok := input.(map[string]interface{})
			return ok && m["clusterID"] == clusterID
		}),
	).Return(db.WorkflowOutbox{}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		uip := c.Conditions.UpdateInProgress
		return c.ID == clusterID &&
			uip != nil &&
			uip.Type == db.OperationTypeOIDCEnable &&
			uip.Message == "enabling OIDC authentication" &&
			uip.StartedAt.After(startedAfter)
	})).Return(db.Cluster{
		ID:   clusterID,
		Name: "test-cluster",
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeOIDCEnable, StartedAt: time.Now()},
		},
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableOidc: &enable},
	})
	require.NoError(t, err)

	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	require.NotNil(t, body.Conditions.UpdateInProgress)
	assert.Equal(t, api.UpdateInProgressTypeOidcEnable, body.Conditions.UpdateInProgress.Type)
	mockDB.MockTx.AssertExpectations(t)
}

func TestClusterService_UpdateCluster_EnableOIDC_RejectsFalse(t *testing.T) {
	disable := false

	mockDB := &mockDB{}
	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: uuid.New(),
		Body:      &api.UpdateClusterRequest{EnableOidc: &disable},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "disabling OIDC authentication is not supported")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableOIDC_RejectsWithEtcdEncryption(t *testing.T) {
	enable := true

	mockDB := &mockDB{}
	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: uuid.New(),
		Body:      &api.UpdateClusterRequest{EnableOidc: &enable, EnableEtcdEncryption: &enable},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "cannot enable OIDC authentication and etcd encryption")
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableOIDC_RejectsWithKubernetesUpgrade(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	enable := true
	newVersion := "1.30.2"

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:                clusterID,
		KubernetesVersion: "1.30.1",
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableOidc: &enable, ControlPlaneKubernetesVersion: &newVersion},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "cannot enable OIDC authentication and update the Kubernetes version")
	mockDB.MockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableOIDC_AlreadyEnabledNoop(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	enable := true

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	alreadyEnabled := db.Cluster{
		ID:              clusterID,
		Name:            "test-cluster",
		OIDCAuthEnabled: true,
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(alreadyEnabled, nil).Once()
	// OIDC already enabled: the toggle is a no-op — no outbox row, no lock taken —
	// but the request still flows through the normal UpdateCluster save.
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID && c.OIDCAuthEnabled && c.Conditions.UpdateInProgress == nil
	})).Return(alreadyEnabled, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableOidc: &enable},
	})
	require.NoError(t, err)

	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	assert.Nil(t, body.Conditions.UpdateInProgress)
	mockDB.MockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableOIDC_RejectsLegacyOperation(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	started := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	enable := true

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:              clusterID,
		OIDCAuthEnabled: false,
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
			ControlPlaneUpgrading: db.Condition{
				Status:             db.ConditionStatusTrue,
				Message:            "upgrade in flight",
				LastTransitionTime: started,
			},
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableOidc: &enable},
	})
	require.NoError(t, err)

	conflict, ok := resp.(api.UpdateCluster409JSONResponse)
	require.True(t, ok, "expected UpdateCluster409JSONResponse, got %T", resp)
	assert.Contains(t, conflict.Message, "k8s_upgrade")
	mockDB.MockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_EnableEtcdEncryption_StartsWorkflowForCAPIBackedCluster(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	enable := true

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		EtcdEncrypted: false,
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:    db.Condition{Status: db.ConditionStatusFalse},
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.Nil,
		},
	}, nil).Once()
	mockDB.MockTx.On(
		"CreateWorkflowOutboxEntry",
		mock.Anything,
		constants.EnableEtcdEncryptionWorkflowType,
		constants.EnableEtcdEncryptionWorkflowPrefix+clusterID.String(),
		mock.MatchedBy(func(input interface{}) bool {
			m, ok := input.(map[string]interface{})
			_, hasNodePoolID := m["nodePoolID"]
			return ok && m["clusterID"] == clusterID && !hasNodePoolID
		}),
	).Return(db.WorkflowOutbox{}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		uip := c.Conditions.UpdateInProgress
		return c.ID == clusterID &&
			!c.EtcdEncrypted &&
			uip != nil &&
			uip.Type == db.OperationTypeEtcdEncryption
	})).Return(db.Cluster{
		ID:            clusterID,
		EtcdEncrypted: false,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption, StartedAt: time.Now()},
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.Nil,
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{EnableEtcdEncryption: &enable},
	})
	require.NoError(t, err)

	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	require.NotNil(t, body.Conditions.UpdateInProgress)
	assert.Equal(t, api.UpdateInProgressTypeEtcdEncryption, body.Conditions.UpdateInProgress.Type)
	mockDB.MockTx.AssertExpectations(t)
}

// TestClusterService_GetCluster_ExposesUpdateInProgress confirms the API
// response includes the UpdateInProgress field so fleet dashboards and
// nvkectl can surface what operation is running and when it started.
func TestClusterService_GetCluster_ExposesUpdateInProgress(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	started := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	mockDB := &mockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:   clusterID,
		Name: "test-cluster",
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{
				Type:      db.OperationTypeEtcdEncryption,
				Message:   "enabling etcd encryption",
				StartedAt: started,
			},
		},
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.GetCluster(context.Background(), api.GetClusterRequestObject{ClusterID: clusterID})
	require.NoError(t, err)

	body, ok := resp.(api.GetCluster200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	require.NotNil(t, body.Conditions.UpdateInProgress, "updateInProgress must be exposed on API response")
	assert.Equal(t, api.UpdateInProgressTypeEtcdEncryption, body.Conditions.UpdateInProgress.Type)
	require.NotNil(t, body.Conditions.UpdateInProgress.Message)
	assert.Equal(t, "enabling etcd encryption", *body.Conditions.UpdateInProgress.Message)
	assert.True(t, body.Conditions.UpdateInProgress.StartedAt.Equal(started))
	mockDB.AssertExpectations(t)
}

func TestClusterService_GetCluster_ExposesUpdateFailed(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	updateFailed := db.NewCondition(true, "Etcd encryption update failed: node preflight failed")

	mockDB := &mockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:   clusterID,
		Name: "test-cluster",
		Conditions: db.ClusterConditions{
			UpdateFailed: &updateFailed,
		},
		Nico: &db.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
	}, nil).Once()

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.GetCluster(context.Background(), api.GetClusterRequestObject{ClusterID: clusterID})
	require.NoError(t, err)

	body, ok := resp.(api.GetCluster200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	require.NotNil(t, body.Conditions.UpdateFailed, "updateFailed must be exposed on API response")
	assert.Equal(t, api.ConditionStatusTrue, body.Conditions.UpdateFailed.Status)
	assert.Equal(t, "Etcd encryption update failed: node preflight failed", body.Conditions.UpdateFailed.Message)
	mockDB.AssertExpectations(t)
}

func TestClusterService_UpdateCluster_ControlPlaneConfig(t *testing.T) {
	clusterID := uuid.New()
	customInst := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000099")
	maxSurge := 1
	prefs := []openapi_types.UUID{customInst}
	provisioningMonitorDisabled := true
	createdCfgID := uuid.New()

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeNico,
		ControlPlaneConfig: db.ControlPlaneConfig{
			MaxSurge: 0,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				Nico: &db.NicoControlPlaneProvider{Release: "ubuntu", ConfigProfile: "default"},
			},
		},
		Nico: &db.NicoClusterProvider{
			SiteID: uuid.New(),
		},
	}, nil).Once()
	mockDB.MockTx.On(
		"ListNodePools",
		mock.Anything,
		db.NodePoolFilters{ClusterID: &clusterID},
	).Return([]db.NodePool{}, nil).Once()
	mockDB.MockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.AnythingOfType("*db.ControlPlaneConfiguration")).Return(db.ControlPlaneConfiguration{
		ID:        createdCfgID,
		ClusterID: clusterID,
		Config: db.ControlPlaneConfigSpec{
			ProvisioningMonitorDisabled: true,
			Nico: &db.NicoControlPlaneProvider{
				InstanceTypePreference: []uuid.UUID{customInst},
			},
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		cpNico, err := c.ControlPlaneConfig.NicoProvider()
		return err == nil &&
			c.ControlPlaneConfig.MaxSurge == 1 &&
			len(cpNico.InstanceTypePreference) == 1 &&
			cpNico.InstanceTypePreference[0] == customInst &&
			c.ControlPlaneConfig.ProvisioningMonitorDisabled
	})).Return(db.Cluster{
		ID: clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: createdCfgID,
			MaxSurge:        1,
		},
	}, nil).Once()
	mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, createdCfgID).Return(db.ControlPlaneConfiguration{
		ID:        createdCfgID,
		ClusterID: clusterID,
		Config: db.ControlPlaneConfigSpec{
			ProvisioningMonitorDisabled: true,
			Nico: &db.NicoControlPlaneProvider{
				InstanceTypePreference: []uuid.UUID{customInst},
			},
		},
	}, nil).Once()

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body: &api.UpdateClusterRequest{
			ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
				MaxSurge:                    &maxSurge,
				Nico:                        &api.NicoControlPlaneProviderUpdate{InstanceTypePreference: prefs},
				ProvisioningMonitorDisabled: &provisioningMonitorDisabled,
			},
		},
	})
	require.NoError(t, err)
	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	require.NotNil(t, body.ControlPlaneConfig)
	require.NotNil(t, body.ControlPlaneConfig.MaxSurge)
	assert.Equal(t, 1, *body.ControlPlaneConfig.MaxSurge)
	require.NotNil(t, body.ControlPlaneConfig.ProvisioningMonitorDisabled)
	assert.True(t, *body.ControlPlaneConfig.ProvisioningMonitorDisabled)
	mockDB.AssertExpectations(t)
}

func TestClusterService_UpdateCluster_ControlPlaneConfig_CreatesSnapshotOnVersionChange(t *testing.T) {
	clusterID := uuid.New()
	oldCfgID := uuid.New()
	newCfgID := uuid.New()
	newVersion := "1.31.0"

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:                clusterID,
		KubernetesVersion: "1.30.0",
		ControlPlaneVIP:   "172.31.0.10",
		ProviderType:      db.ProviderTypeCAPI,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: oldCfgID,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				KubernetesVersion: "1.30.0",
			},
		},
	}, nil).Once()
	controlPlaneFalse := false
	mockDB.MockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlaneFalse,
	}).Return([]db.NodePool{}, nil).Once()
	mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, oldCfgID).Return(db.ControlPlaneConfiguration{
		ID:        oldCfgID,
		ClusterID: clusterID,
		Config: db.ControlPlaneConfigSpec{
			KubernetesVersion: "1.30.0",
		},
	}, nil).Once()
	mockDB.MockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
		return row.ClusterID == clusterID && row.Config.KubernetesVersion == newVersion
	})).Return(db.ControlPlaneConfiguration{
		ID:        newCfgID,
		ClusterID: clusterID,
		Config: db.ControlPlaneConfigSpec{
			KubernetesVersion: newVersion,
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID &&
			c.KubernetesVersion == newVersion &&
			c.ControlPlaneConfig.ConfigurationID == newCfgID
	})).Return(db.Cluster{
		ID:                clusterID,
		KubernetesVersion: newVersion,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: newCfgID,
		},
	}, nil).Once()
	mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, newCfgID).Return(db.ControlPlaneConfiguration{
		ID:        newCfgID,
		ClusterID: clusterID,
		Config: db.ControlPlaneConfigSpec{
			KubernetesVersion: newVersion,
		},
	}, nil).Once()

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body: &api.UpdateClusterRequest{
			ControlPlaneKubernetesVersion: &newVersion,
		},
	})
	require.NoError(t, err)
	body, ok := resp.(api.UpdateCluster200JSONResponse)
	require.True(t, ok, "expected UpdateCluster200JSONResponse, got %T", resp)
	assert.Equal(t, newVersion, body.ControlPlaneKubernetesVersion)
	mockDB.AssertExpectations(t)
}

func TestClusterService_UpdateCluster_ControlPlaneConfig_RejectsPodCIDRCapacityExceeded(t *testing.T) {
	clusterID := uuid.New()
	cfgID := uuid.New()
	maxSurge := 1

	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:           clusterID,
		PodCIDR:      "10.240.0.0/23",
		ServiceCIDR:  "10.120.0.0/24",
		ProviderType: db.ProviderTypeCAPI,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: cfgID,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				Capi: &db.CAPIControlPlaneProvider{},
			},
		},
	}, nil).Once()
	mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, cfgID).Return(db.ControlPlaneConfiguration{
		ID:        cfgID,
		ClusterID: clusterID,
		Config: db.ControlPlaneConfigSpec{
			Capi: &db.CAPIControlPlaneProvider{},
		},
	}, nil).Once()
	mockDB.MockTx.On(
		"ListNodePools",
		mock.Anything,
		db.NodePoolFilters{ClusterID: &clusterID},
	).Return([]db.NodePool{{
		ClusterID:    clusterID,
		DesiredCount: 1,
		MaxSurge:     1,
	}}, nil).Once()

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body: &api.UpdateClusterRequest{
			ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{MaxSurge: &maxSurge},
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "provides 2 /24 node blocks, but 3 are required")
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClusterService_UpdateCluster_ControlPlaneConfig_RejectsMaxSurgeAboveOne(t *testing.T) {
	clusterID := uuid.New()
	maxSurge := 2

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeCAPI,
	}, nil).Once()

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body: &api.UpdateClusterRequest{
			ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
				MaxSurge: &maxSurge,
			},
		},
	})
	require.NoError(t, err)
	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "maxSurge should be max 1")
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

// TestClusterService_UpdateCluster_ControlPlaneConfig_RejectsMismatchedProvider
// covers the gate that rejects a controlPlaneConfig.nico/.capi that doesn't
// match the cluster's own provider, before any field is touched.
func TestClusterService_UpdateCluster_ControlPlaneConfig_RejectsMismatchedProvider(t *testing.T) {
	newMockDB := func(cluster db.Cluster) *mockDB {
		mockDB := &mockDB{}
		mockDB.MockTx = &mockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
		mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
		return mockDB
	}

	assertRejected := func(t *testing.T, mockDB *mockDB, body *api.UpdateClusterControlPlaneConfig, clusterID uuid.UUID, wantMsg string) {
		t.Helper()
		svc := newTestClusterService(discardLogger(), mockDB)
		resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
			ClusterID: clusterID,
			Body:      &api.UpdateClusterRequest{ControlPlaneConfig: body},
		})
		require.NoError(t, err)
		badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
		require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
		assert.Contains(t, badRequest.Message, wantMsg)
		mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
	}

	t.Run("infra on a nico cluster is rejected", func(t *testing.T) {
		clusterID := uuid.New()
		mockDB := newMockDB(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeNico})
		assertRejected(t, mockDB, &api.UpdateClusterControlPlaneConfig{
			Infra: &api.InfraControlPlaneProviderUpdate{},
		}, clusterID, "controlPlaneConfig.infra is not supported, please provide controlPlaneConfig.nico")
	})

	t.Run("nico on a CAPI cluster is rejected", func(t *testing.T) {
		clusterID := uuid.New()
		mockDB := newMockDB(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeCAPI})
		assertRejected(t, mockDB, &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{},
		}, clusterID, "controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
	})

	t.Run("both nico and infra on a nico cluster is rejected", func(t *testing.T) {
		clusterID := uuid.New()
		mockDB := newMockDB(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeNico})
		assertRejected(t, mockDB, &api.UpdateClusterControlPlaneConfig{
			Nico:  &api.NicoControlPlaneProviderUpdate{},
			Infra: &api.InfraControlPlaneProviderUpdate{},
		}, clusterID, "controlPlaneConfig.infra is not supported, please provide controlPlaneConfig.nico")
	})

	t.Run("both nico and infra on a CAPI cluster is rejected", func(t *testing.T) {
		clusterID := uuid.New()
		mockDB := newMockDB(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeCAPI})
		assertRejected(t, mockDB, &api.UpdateClusterControlPlaneConfig{
			Nico:  &api.NicoControlPlaneProviderUpdate{},
			Infra: &api.InfraControlPlaneProviderUpdate{},
		}, clusterID, "controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
	})

	// Regression guard: instanceTypePreference is handled by a code path
	// (applyClusterControlPlaneConfigUpdate's hasInstanceTypePreferences block) that
	// never reached applyClusterLevelControlPlaneSpecUpdate's old, now-removed check
	// when it was the only field set under nico, so a CAPI cluster could silently
	// accept and drop it instead of getting a 400.
	t.Run("nico.instanceTypePreference alone on a CAPI cluster is rejected", func(t *testing.T) {
		clusterID := uuid.New()
		mockDB := newMockDB(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeCAPI})
		assertRejected(t, mockDB, &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{
				InstanceTypePreference: []openapi_types.UUID{uuid.New()},
			},
		}, clusterID, "controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
	})
}

func TestClusterService_UpdateCluster_ControlPlaneConfig_RejectsUnsupportedProviderPayload(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &mockDB{MockTx: &mockTx{}}
	mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeNico,
	}, nil).Once()

	nodePoolService := server.NewNodePoolService(discardLogger(), mockDB, config.TeleportConfig{}, []string{config.InfraProviderInfra})
	svc := server.NewClusterService(
		discardLogger(),
		mockDB,
		nodePoolService,
		config.TeleportConfig{},
		false,
		[]string{config.InfraProviderInfra},
		false,
		"",
	)

	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body: &api.UpdateClusterRequest{
			ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
				Nico: &api.NicoControlPlaneProviderUpdate{
					InstanceTypePreference: []openapi_types.UUID{uuid.New()},
				},
			},
		},
	})
	require.NoError(t, err)

	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	assert.Contains(t, badRequest.Message, "nico provider is not supported by this deployment")
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

// TestClusterService_UpdateCluster_NotFound confirms UpdateCluster surfaces
// 404 when the cluster row is missing — regression guard for the
// GetCluster → GetClusterForUpdate swap.
func TestClusterService_UpdateCluster_NotFound(t *testing.T) {
	clusterID := uuid.New()

	mockDB := &mockDB{}
	mockDB.MockTx = &mockTx{}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	svc := newTestClusterService(discardLogger(), mockDB)
	resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
		ClusterID: clusterID,
		Body:      &api.UpdateClusterRequest{},
	})
	require.NoError(t, err)
	assert.IsType(t, api.UpdateCluster404Response{}, resp)
}

// baseNicoClusterCreateRequest returns a fully valid Nico cluster create request.
func baseNicoClusterCreateRequest() *api.CreateClusterRequest {
	return &api.CreateClusterRequest{
		Name: "runtime-dgxc-k8s-forge-cp",
		Nico: &api.NicoClusterProvider{
			OrgID:  "test-org",
			SiteID: uuid.New(),
			VpcID:  uuid.New(),
		},
		CniPlugin:                     "calico",
		ControlPlaneKubernetesVersion: "1.29.0",
		ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
	}
}

func TestValidateCreateClusterRequestFields_LegacyCalicoCreationGate(t *testing.T) {
	tests := []struct {
		name        string
		cni         api.CreateClusterRequestCniPlugin
		allowCalico bool
		wantErr     string
	}{
		{name: "omitted CNI defaults to OVN"},
		{name: "explicit OVN", cni: api.CreateClusterRequestCniPluginOvnKubernetes},
		{
			name:    "unknown CNI only advertises OVN",
			cni:     "flannel",
			wantErr: "invalid CNI plugin: flannel, valid plugins: [ovn-kubernetes]",
		},
		{
			name:    "Calico disabled by default",
			cni:     api.CreateClusterRequestCniPluginCalico,
			wantErr: "calico cluster creation is disabled by this controller deployment; use ovn-kubernetes",
		},
		{name: "Calico enabled for legacy fleet", cni: api.CreateClusterRequestCniPluginCalico, allowCalico: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := server.NewClusterService(discardLogger(), nil, nil, config.TeleportConfig{}, false, nil, tt.allowCalico, "")
			req := baseNicoClusterCreateRequest()
			req.CniPlugin = tt.cni

			err := svc.ValidateCreateClusterRequestFields(req)

			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestClusterService_CreateClusterRejectsCalicoBeforeTransaction(t *testing.T) {
	mockDB := &mockDB{}
	svc := server.NewClusterService(discardLogger(), mockDB, nil, config.TeleportConfig{}, false, nil, false, "")

	resp, err := svc.CreateCluster(context.Background(), api.CreateClusterRequestObject{Body: baseNicoClusterCreateRequest()})

	require.NoError(t, err)
	badRequest, ok := resp.(api.CreateCluster400JSONResponse)
	require.True(t, ok, "expected CreateCluster400JSONResponse, got %T", resp)
	assert.Equal(t, "calico cluster creation is disabled by this controller deployment; use ovn-kubernetes", badRequest.Message)
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

func TestValidateCreateClusterRequestFields_RequiresExactlyOneProvider(t *testing.T) {
	svc := server.NewClusterService(discardLogger(), nil, nil, config.TeleportConfig{}, false, nil, false, "")

	t.Run("neither provider", func(t *testing.T) {
		req := baseNicoClusterCreateRequest()
		req.Nico = nil
		require.EqualError(t, svc.ValidateCreateClusterRequestFields(req), "exactly one provider must be set")
	})

	t.Run("both providers", func(t *testing.T) {
		req := baseNicoClusterCreateRequest()
		req.Infra = &api.InfraClusterProvider{
			Cluster: map[string]interface{}{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
				"kind":       "DockerCluster",
			},
		}
		require.EqualError(t, svc.ValidateCreateClusterRequestFields(req), "exactly one provider must be set")
	})
}

// The Forge identity fields are required, and they are only reachable through
// the nested nico payload.
func TestValidateCreateClusterRequestFields_NicoIdentityFieldsRequired(t *testing.T) {
	svc := server.NewClusterService(discardLogger(), nil, nil, config.TeleportConfig{}, false, nil, false, "")

	cases := []struct {
		name    string
		mutate  func(*api.NicoClusterProvider)
		wantErr string
	}{
		{
			name:    "siteID",
			mutate:  func(n *api.NicoClusterProvider) { n.SiteID = uuid.Nil },
			wantErr: "nico.siteID is required to create a nico cluster",
		},
		{
			name:    "orgID",
			mutate:  func(n *api.NicoClusterProvider) { n.OrgID = "" },
			wantErr: "nico.orgID is required to create a nico cluster",
		},
		{
			name:    "vpcID",
			mutate:  func(n *api.NicoClusterProvider) { n.VpcID = uuid.Nil },
			wantErr: "nico.vpcID is required to create a nico cluster",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseNicoClusterCreateRequest()
			tc.mutate(req.Nico)
			require.EqualError(t, svc.ValidateCreateClusterRequestFields(req), tc.wantErr)
		})
	}
}

func TestDbClusterFromAPI_NestedProvidersReachTheStoredCluster(t *testing.T) {
	t.Run("nico", func(t *testing.T) {
		siteID := uuid.New()
		vpcID := uuid.New()
		req := &api.CreateClusterRequest{
			Name:                          "prod",
			ControlPlaneKubernetesVersion: "1.31.0",
			CniPlugin:                     "cilium",
			Nico:                          &api.NicoClusterProvider{OrgID: "org", SiteID: siteID, VpcID: vpcID},
		}

		cluster, err := server.DBClusterFromAPI(req)
		require.NoError(t, err)
		require.Equal(t, db.ProviderTypeNico, cluster.ProviderType)
		require.NotNil(t, cluster.Nico)
		assert.Equal(t, "org", cluster.Nico.OrgID)
		assert.Equal(t, siteID, cluster.Nico.SiteID)
		assert.Equal(t, vpcID, cluster.Nico.VpcID)
		assert.Nil(t, cluster.Capi)

		apiCluster := server.APIClusterFromDB(&cluster)
		require.NotNil(t, apiCluster.Nico)
		assert.Equal(t, "org", apiCluster.Nico.OrgID)
		assert.Equal(t, siteID, apiCluster.Nico.SiteID)
		assert.Nil(t, apiCluster.Infra)
	})

	t.Run("infra", func(t *testing.T) {
		infra := map[string]interface{}{
			"kind":       "DockerCluster",
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		}
		req := &api.CreateClusterRequest{
			Name:                          "lab",
			ControlPlaneKubernetesVersion: "1.31.0",
			CniPlugin:                     "cilium",
			Infra:                         &api.InfraClusterProvider{Cluster: infra},
		}

		cluster, err := server.DBClusterFromAPI(req)
		require.NoError(t, err)
		require.Equal(t, db.ProviderTypeCAPI, cluster.ProviderType)
		require.NotNil(t, cluster.Capi)
		assert.Nil(t, cluster.Nico)

		apiCluster := server.APIClusterFromDB(&cluster)
		require.NotNil(t, apiCluster.Infra)
		assert.Equal(t, "DockerCluster", apiCluster.Infra.Cluster["kind"])
		assert.Nil(t, apiCluster.Nico)
	})
}

func TestApiClusterFromDB_OmitsMissingProviderPayload(t *testing.T) {
	// The nico mapper reads the JSONB payload, so a row whose payload has not
	// been stamped yet emits nico:null rather than inventing one.
	cluster := db.Cluster{
		ID:   uuid.New(),
		Name: "unstamped",
	}
	got := server.APIClusterFromDB(&cluster)
	assert.Nil(t, got.Nico)
}
