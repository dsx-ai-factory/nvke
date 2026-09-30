// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// validForgeControlPlaneConfig returns a minimal valid cluster-level control
// plane config for a Forge-backed cluster.
func validForgeControlPlaneConfig() *api.CreateClusterControlPlaneConfig {
	return &api.CreateClusterControlPlaneConfig{
		DesiredCount: 3,
		Nico: &api.NicoControlPlaneProviderCreate{
			InstanceTypePreference: []uuid.UUID{uuid.New()},
			OSProfile:              "ubuntu",
			ConfigProfile:          "default",
		},
	}
}

func validForgeCreateClusterRequest() *api.CreateClusterRequest {
	return &api.CreateClusterRequest{
		Name: "runtime-dgxc-k8s-forge-cp",
		Nico: &api.NicoClusterProvider{
			OrgID:  "test-org",
			SiteID: uuid.New(),
			VpcID:  uuid.New(),
		},
		CniPlugin:                     "calico",
		ControlPlaneKubernetesVersion: "1.29.0",
	}
}

// validForgeNodePoolCreateConfig returns a minimal valid embedded node pool for
// a Forge-backed cluster.
func validForgeNodePoolCreateConfig(name string) api.NodePoolCreateConfig {
	return api.NodePoolCreateConfig{
		Name:           name,
		NodeNamePrefix: name,
		Nico: &api.NicoNodePoolProvider{
			SubnetIDs:      []uuid.UUID{uuid.New()},
			InstanceTypeID: uuid.New(),
			OSProfile:      "ubuntu",
			ConfigProfile:  "default",
		},
		ControlPlane:     true,
		DesiredNodeCount: 1,
	}
}

func TestValidateCreateClusterRequestFields_ControlPlaneControllerFlag(t *testing.T) {
	t.Run("flag OFF rejects cluster-level controlPlaneConfig", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: false}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()
		// Provide a control-plane node pool so the legacy "must have a CP node" rule passes.
		req.NodePools = &[]api.NodePoolCreateConfig{validForgeNodePoolCreateConfig("cp")}

		err := svc.ValidateCreateClusterRequestFields(req)
		require.ErrorIs(t, err, errControlPlaneConfigNotEnabled)
	})

	t.Run("flag ON rejects control-plane node pools", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()
		req.NodePools = &[]api.NodePoolCreateConfig{validForgeNodePoolCreateConfig("control-plane")}

		err := svc.ValidateCreateClusterRequestFields(req)
		require.ErrorIs(t, err, errControlPlaneNodePoolNotAllowed)
	})

	t.Run("flag ON requires controlPlaneConfig", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()

		err := svc.ValidateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "controlPlaneConfig is required")
	})

	t.Run("flag ON rejects desiredCount below 1", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		cp := validForgeControlPlaneConfig()
		cp.DesiredCount = 0
		req.ControlPlaneConfig = cp

		err := svc.ValidateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "desiredCount must be at least 1")
	})

	t.Run("flag ON accepts a valid Forge cluster-level control plane with no node pools", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		cp := validForgeControlPlaneConfig()
		cp.DesiredCount = 1
		req.ControlPlaneConfig = cp

		err := svc.ValidateCreateClusterRequestFields(req)
		require.NoError(t, err)
	})

	t.Run("flag ON rejects multiple control-plane nodes without a VIP", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()

		err := svc.ValidateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "controlPlaneVIP")
	})

	t.Run("flag ON accepts multiple control-plane nodes with a VIP", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneVIP = "172.31.0.10"
		req.ControlPlaneConfig = validForgeControlPlaneConfig()

		err := svc.ValidateCreateClusterRequestFields(req)
		require.NoError(t, err)
	})

	t.Run("flag OFF accepts one control-plane node without a VIP", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: false}
		req := validForgeCreateClusterRequest()
		req.NodePools = &[]api.NodePoolCreateConfig{validForgeNodePoolCreateConfig("cp")}

		err := svc.ValidateCreateClusterRequestFields(req)
		require.NoError(t, err)
	})

	t.Run("flag OFF rejects aggregate control-plane capacity above one without a VIP", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: false}
		req := validForgeCreateClusterRequest()
		first := validForgeNodePoolCreateConfig("cp-a")
		second := validForgeNodePoolCreateConfig("cp-b")
		req.NodePools = &[]api.NodePoolCreateConfig{first, second}

		err := svc.ValidateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "controlPlaneVIP")
	})

	t.Run("flag OFF accepts aggregate control-plane capacity above one with a VIP", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: false}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneVIP = "172.31.0.10"
		first := validForgeNodePoolCreateConfig("cp-a")
		second := validForgeNodePoolCreateConfig("cp-b")
		req.NodePools = &[]api.NodePoolCreateConfig{first, second}

		err := svc.ValidateCreateClusterRequestFields(req)
		require.NoError(t, err)
	})

	t.Run("flag ON rejects a nil instance type ID in instanceTypePreference", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		cp := validForgeControlPlaneConfig()
		// A nil UUID passes the length check but would be silently dropped by
		// dedupeUUIDsPreserveOrder, so it must be rejected up front.
		cp.Nico.InstanceTypePreference = []uuid.UUID{uuid.New(), uuid.Nil}
		req.ControlPlaneConfig = cp

		err := svc.ValidateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid nil instance type ID")
	})

	t.Run("flag ON rejects a Forge control plane payload on a CAPI cluster", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := &api.CreateClusterRequest{
			Name:                          "capi-cluster",
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			Infra: &api.InfraClusterProvider{
				Cluster: map[string]interface{}{
					"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
					"kind":       "DockerCluster",
				},
			},
			ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{
				DesiredCount: 3,
				Nico:         &api.NicoControlPlaneProviderCreate{OSProfile: "ubuntu"},
			},
		}

		err := svc.ValidateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
	})
}

func TestApplyClusterLevelControlPlaneSpecUpdate_DesiredCount(t *testing.T) {
	svc := clusterService{useControlPlaneController: true}
	ctx := context.Background()

	newCAPICluster := func(current int, vip string) *db.Cluster {
		return &db.Cluster{
			ID:              uuid.New(),
			ControlPlaneVIP: vip,
			ControlPlaneConfig: db.ControlPlaneConfig{
				DesiredCount: current,
			},
			Nico: &db.NicoClusterProvider{
				SiteID: uuid.Nil, // CAPI cluster avoids Forge networking lookups.
			},
		}
	}

	t.Run("allows scale-down below current", func(t *testing.T) {
		cluster := newCAPICluster(3, "")
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 2
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.NoError(t, err)
		require.Equal(t, 2, cluster.ControlPlaneConfig.DesiredCount, "scale-down must be accepted and persisted")
	})

	t.Run("allows equal desiredCount", func(t *testing.T) {
		cluster := newCAPICluster(3, "")
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 3
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.NoError(t, err)
		require.Equal(t, 3, cluster.ControlPlaneConfig.DesiredCount)
	})

	t.Run("rejects scale-up without a VIP", func(t *testing.T) {
		cluster := newCAPICluster(3, "")
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 5
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.Error(t, err)
		require.Contains(t, err.Error(), "controlPlaneVIP")
		require.Equal(t, 3, cluster.ControlPlaneConfig.DesiredCount)
	})

	t.Run("allows scale-up with a VIP", func(t *testing.T) {
		cluster := newCAPICluster(3, "172.31.0.10")
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 5
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.NoError(t, err)
		require.Equal(t, 5, cluster.ControlPlaneConfig.DesiredCount)
	})

	t.Run("rejects desiredCount below 1", func(t *testing.T) {
		cluster := newCAPICluster(3, "")
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 0
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.Error(t, err)
		require.Contains(t, err.Error(), "desiredCount must be at least 1")
	})
}

func TestApplyClusterControlPlaneConfigUpdate_MaxSurgeWithoutVIP(t *testing.T) {
	svc := clusterService{useControlPlaneController: true}
	cluster := &db.Cluster{
		ID: uuid.New(),
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount: 1,
		},
	}
	maxSurge := 1

	err := svc.applyClusterControlPlaneConfigUpdate(context.Background(), nil, cluster, &api.UpdateClusterControlPlaneConfig{
		MaxSurge: &maxSurge,
	})
	require.NoError(t, err)
	require.Equal(t, maxSurge, cluster.ControlPlaneConfig.MaxSurge)
}

func TestApplyClusterControlPlaneConfigUpdate_RolloutRequiresVIP(t *testing.T) {
	for _, tc := range []struct {
		name            string
		controlPlaneVIP string
		wantBadRequest  bool
	}{
		{name: "without VIP", wantBadRequest: true},
		{name: "with VIP", controlPlaneVIP: "172.31.0.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldConfigID := uuid.New()
			newConfigID := uuid.New()
			instanceTypeID := uuid.New()
			currentSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
			currentSpec.SetNico(db.NicoControlPlaneProvider{
				InstanceTypePreference: []uuid.UUID{instanceTypeID},
				OSProfile:              "os-v1",
				ConfigProfile:          "cfg-v1",
			})
			cluster := &db.Cluster{
				ID:                uuid.New(),
				ProviderType:      db.ProviderTypeNico,
				Nico:              &db.NicoClusterProvider{},
				KubernetesVersion: "1.34.2",
				ControlPlaneVIP:   tc.controlPlaneVIP,
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID:        oldConfigID,
					DesiredCount:           1,
					ControlPlaneConfigSpec: currentSpec,
				},
			}
			newProfile := "cfg-v2"
			tx := &testutil.MockTx{}
			currentConfigCall := tx.On("GetControlPlaneConfiguration", mock.Anything, oldConfigID).Return(db.ControlPlaneConfiguration{
				ID: oldConfigID, ClusterID: cluster.ID, Config: currentSpec,
			}, nil)
			createConfigCall := tx.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
				provider, err := row.Config.NicoProvider()
				return err == nil && provider.ConfigProfile == newProfile
			})).Return(db.ControlPlaneConfiguration{
				ID: newConfigID, ClusterID: cluster.ID,
			}, nil)
			if tc.wantBadRequest {
				currentConfigCall.Maybe()
				createConfigCall.Maybe()
			} else {
				currentConfigCall.Once()
				createConfigCall.Once()
			}

			svc := clusterService{useControlPlaneController: true}
			err := svc.applyClusterControlPlaneConfigUpdate(context.Background(), tx, cluster, &api.UpdateClusterControlPlaneConfig{
				Nico: &api.NicoControlPlaneProviderUpdate{ConfigProfile: &newProfile},
			})
			if tc.wantBadRequest {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "controlPlaneVIP")
				tx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
				return
			}
			require.NoError(t, err)
			tx.AssertExpectations(t)
		})
	}
}

func TestCreateNodePool_RejectsControlPlanePoolWhenControllerEnabled(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Nico: &db.NicoClusterProvider{
			OrgID:  "test-org",
			SiteID: uuid.New(),
		},
	}, nil)

	service := NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, nil, true)

	resp, err := service.CreateNodePool(context.Background(), api.CreateNodePoolRequestObject{
		Body: &api.CreateNodePoolRequest{
			ClusterID:      clusterID,
			Name:           "cp",
			NodeNamePrefix: "cp",
			Nico: &api.NicoNodePoolProvider{
				OSProfile:     "ubuntu",
				ConfigProfile: "default",
			},
			ControlPlane:     true,
			DesiredNodeCount: 1,
		},
	})
	require.NoError(t, err)
	badReq, ok := resp.(api.CreateNodePool400JSONResponse)
	require.True(t, ok, "expected a 400 response, got %T", resp)
	require.Contains(t, badReq.Message, "control-plane node pools are not allowed")
}

// TestGetClusterControlPlane covers the read-only control-plane resource:
// it returns the persisted config plus an aggregate status for both
// node-pool-managed and controller-managed control planes, and 404s when the
// cluster does not exist.
func TestGetClusterControlPlane(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
	instanceType := uuid.MustParse("550e8400-e29b-41d4-a716-446655440003")

	newCluster := func() db.Cluster {
		cluster := db.Cluster{
			ID:                clusterID,
			KubernetesVersion: "1.29.0",
			ControlPlaneConfig: db.ControlPlaneConfig{
				DesiredCount: 3,
			},
			Conditions: db.ClusterConditions{
				ControlPlaneUpgrading: db.NewCondition(true, "upgrading to 1.29.0"),
				CPNodesRecycling:      db.NewCondition(false, "not recycling"),
			},
		}
		cluster.ControlPlaneConfig.SetNico(db.NicoControlPlaneProvider{
			InstanceTypePreference: []uuid.UUID{instanceType},
			OSProfile:              "ubuntu",
			ConfigProfile:          "default",
		})
		return cluster
	}

	cpFilter := func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID &&
			f.ControlPlane != nil && *f.ControlPlane
	}

	t.Run("node-pool-managed control plane", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(newCluster(), nil)
		mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(cpFilter)).Return([]db.Node{
			{
				ID:           uuid.New(),
				ClusterID:    clusterID,
				NodePoolID:   nodePoolID,
				ControlPlane: true,
				Conditions:   db.NodeConditions{KubernetesReady: db.NewCondition(true, "ready")},
			},
			{
				ID:           uuid.New(),
				ClusterID:    clusterID,
				NodePoolID:   nodePoolID,
				ControlPlane: true,
				Conditions:   db.NodeConditions{KubernetesReady: db.NewCondition(false, "not ready")},
			},
		}, nil)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlane(ctx, api.GetClusterControlPlaneRequestObject{ClusterID: clusterID})
		require.NoError(t, err)

		out, ok := resp.(api.GetClusterControlPlane200JSONResponse)
		require.True(t, ok, "expected 200 response, got %T", resp)
		require.Equal(t, 3, out.Config.DesiredCount)
		require.NotNil(t, out.Config.Nico)
		require.Equal(t, "ubuntu", out.Config.Nico.OSProfile)
		require.Equal(t, 2, out.Status.NodeCount)
		require.Equal(t, 1, out.Status.ReadyNodeCount)
		require.Equal(t, "1.29.0", out.Status.KubernetesVersion)
		require.Equal(t, api.ConditionStatusTrue, out.Status.ControlPlaneUpgrading.Status)
		require.Equal(t, api.ConditionStatusFalse, out.Status.CpNodesRecycling.Status)
		mockDB.AssertExpectations(t)
	})

	t.Run("controller-managed control plane", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(newCluster(), nil)
		// Controller-managed CP nodes have a nil node pool ID; the read model
		// still surfaces them because ControlPlane is set on the node row.
		mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(cpFilter)).Return([]db.Node{
			{
				ID:           uuid.New(),
				ClusterID:    clusterID,
				NodePoolID:   uuid.Nil,
				ControlPlane: true,
				Conditions:   db.NodeConditions{KubernetesReady: db.NewCondition(true, "ready")},
			},
		}, nil)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlane(ctx, api.GetClusterControlPlaneRequestObject{ClusterID: clusterID})
		require.NoError(t, err)

		out, ok := resp.(api.GetClusterControlPlane200JSONResponse)
		require.True(t, ok, "expected 200 response, got %T", resp)
		require.Equal(t, 1, out.Status.NodeCount)
		require.Equal(t, 1, out.Status.ReadyNodeCount)
		require.Equal(t, 3, out.Config.DesiredCount)
		mockDB.AssertExpectations(t)
	})

	t.Run("controller-managed control plane surfaces a rollout summary", func(t *testing.T) {
		desiredCfgID := uuid.MustParse("aaaaaaaa-0000-0000-0000-0000000000d1")
		driftedCfgID := uuid.MustParse("aaaaaaaa-0000-0000-0000-0000000000d2")
		desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
		desiredSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "os-v2", ConfigProfile: "cfg-v2"})
		driftedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
		driftedSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "os-v1", ConfigProfile: "cfg-v2"})

		cluster := newCluster()
		cluster.EnableCPNodeRecycling = true
		cluster.ControlPlaneConfig.ConfigurationID = desiredCfgID
		cluster.ControlPlaneConfig.ControlPlaneConfigSpec = desiredSpec

		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
		mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(cpFilter)).Return([]db.Node{
			// Pinned to the desired snapshot and Ready -> up to date.
			{
				ID:                          uuid.New(),
				ClusterID:                   clusterID,
				NodePoolID:                  uuid.Nil,
				ControlPlane:                true,
				ControlPlaneConfigurationID: desiredCfgID,
				Conditions:                  db.NodeConditions{KubernetesReady: db.NewCondition(true, "ready")},
			},
			// Pinned to an older snapshot -> pending.
			{
				ID:                          uuid.New(),
				ClusterID:                   clusterID,
				NodePoolID:                  uuid.Nil,
				ControlPlane:                true,
				ControlPlaneConfigurationID: driftedCfgID,
				Conditions:                  db.NodeConditions{KubernetesReady: db.NewCondition(true, "ready")},
			},
			// Pinned to the desired snapshot but not yet Ready -> in flight, so
			// neither up to date nor pending.
			{
				ID:                          uuid.New(),
				ClusterID:                   clusterID,
				NodePoolID:                  uuid.Nil,
				ControlPlane:                true,
				ControlPlaneConfigurationID: desiredCfgID,
				Conditions:                  db.NodeConditions{KubernetesReady: db.NewCondition(false, "coming up")},
			},
		}, nil)
		mockDB.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything).Return(
			[]db.ControlPlaneConfiguration{
				{ID: desiredCfgID, Config: desiredSpec},
				{ID: driftedCfgID, Config: driftedSpec},
			}, nil,
		)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlane(ctx, api.GetClusterControlPlaneRequestObject{ClusterID: clusterID})
		require.NoError(t, err)

		out := resp.(api.GetClusterControlPlane200JSONResponse)
		require.NotNil(t, out.Status.Rollout)
		assert.Equal(t, 3, out.Status.Rollout.ManagedNodeCount)
		assert.Equal(t, 1, out.Status.Rollout.UpToDateNodeCount)   // converged and Ready
		assert.Equal(t, 1, out.Status.Rollout.PendingNodeCount)    // drifted
		assert.Equal(t, 1, out.Status.Rollout.InProgressNodeCount) // converged but not yet Ready
		// The three buckets partition the managed nodes.
		assert.Equal(t, out.Status.Rollout.ManagedNodeCount,
			out.Status.Rollout.UpToDateNodeCount+out.Status.Rollout.PendingNodeCount+out.Status.Rollout.InProgressNodeCount)
		assert.False(t, out.Status.Rollout.InProgress)      // CPNodesRecycling condition is false.
		assert.True(t, out.Status.Rollout.RecyclingEnabled) // recycling enabled -> drift reconciled automatically.
		mockDB.AssertExpectations(t)
	})

	t.Run("node-pool-managed control plane has no rollout summary", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(newCluster(), nil)
		mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(cpFilter)).Return([]db.Node{
			{
				ID:           uuid.New(),
				ClusterID:    clusterID,
				NodePoolID:   nodePoolID,
				ControlPlane: true,
				Conditions:   db.NodeConditions{KubernetesReady: db.NewCondition(true, "ready")},
			},
		}, nil)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlane(ctx, api.GetClusterControlPlaneRequestObject{ClusterID: clusterID})
		require.NoError(t, err)

		out := resp.(api.GetClusterControlPlane200JSONResponse)
		assert.Nil(t, out.Status.Rollout)
		mockDB.AssertExpectations(t)
	})

	t.Run("404 when cluster not found", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlane(ctx, api.GetClusterControlPlaneRequestObject{ClusterID: clusterID})
		require.NoError(t, err)
		require.IsType(t, api.GetClusterControlPlane404Response{}, resp)
		mockDB.AssertExpectations(t)
	})
}

// TestApplyClusterControlPlaneConfigUpdate_RejectsClearingRequiredField proves a
// PATCH that clears a create-time-required control-plane field is rejected before
// the merged spec is persisted (mirroring create-time validation on update).
func TestApplyClusterControlPlaneConfigUpdate_RejectsClearingRequiredField(t *testing.T) {
	ctx := context.Background()

	newForgeCluster := func() *db.Cluster {
		cluster := &db.Cluster{
			ID: uuid.New(),
			ControlPlaneConfig: db.ControlPlaneConfig{
				DesiredCount: 3,
			},
		}
		cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New()})
		cluster.ControlPlaneConfig.SetNico(db.NicoControlPlaneProvider{
			InstanceTypePreference: []uuid.UUID{uuid.New()},
			OSProfile:              "ubuntu",
			ConfigProfile:          "default",
		})
		return cluster
	}

	applyUpdate := func(t *testing.T, cluster *db.Cluster, req *api.UpdateClusterControlPlaneConfig) error {
		t.Helper()
		svc := clusterService{useControlPlaneController: true}
		return svc.applyClusterControlPlaneConfigUpdate(ctx, nil, cluster, req)
	}

	// tx is never reached: validation fails before the config row is persisted.
	t.Run("clearing osProfile is rejected", func(t *testing.T) {
		empty := ""
		err := applyUpdate(t, newForgeCluster(), &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{OSProfile: &empty},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "osProfile is required")
	})

	t.Run("clearing configProfile is rejected", func(t *testing.T) {
		empty := ""
		err := applyUpdate(t, newForgeCluster(), &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{ConfigProfile: &empty},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "configProfile is required")
	})

	// instanceTypePreference is merged in the parent (not applyClusterLevelControlPlaneSpecUpdate),
	// so this exercises the parent-level merged-spec validation.
	t.Run("clearing instanceTypePreference is rejected", func(t *testing.T) {
		err := applyUpdate(t, newForgeCluster(), &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{InstanceTypePreference: []uuid.UUID{}},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "instanceTypePreference must contain at least one")
	})

	t.Run("nil instance type ID in instanceTypePreference is rejected", func(t *testing.T) {
		err := applyUpdate(t, newForgeCluster(), &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{
				InstanceTypePreference: []uuid.UUID{uuid.New(), uuid.Nil},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid nil instance type ID")
	})
}

func TestControlPlaneConfigSpecFromCreateAPI_NestedProviders(t *testing.T) {
	prefs := []uuid.UUID{uuid.New()}
	subnet := uuid.New()
	tmpl := map[string]interface{}{"kind": "DockerMachineTemplate"}

	t.Run("nested nico lands on the stored nico payload", func(t *testing.T) {
		cfg := &api.CreateClusterControlPlaneConfig{
			DesiredCount: 3,
			Nico: &api.NicoControlPlaneProviderCreate{
				InstanceTypePreference: prefs,
				SubnetIDs:              []uuid.UUID{subnet},
				OSProfile:              "os",
				ConfigProfile:          "cfg",
				SSHKeyGroupIDs:         []uuid.UUID{uuid.New()},
			},
		}

		spec := controlPlaneConfigSpecFromCreateAPI(cfg, "1.31.0", true, db.ProviderTypeNico)
		require.NotNil(t, spec.Nico)
		assert.Nil(t, spec.Capi)
		assert.Equal(t, prefs, spec.Nico.InstanceTypePreference)
		assert.Equal(t, []uuid.UUID{subnet}, spec.Nico.SubnetIDs)
		assert.Equal(t, "os", spec.Nico.OSProfile)
		assert.Equal(t, "cfg", spec.Nico.ConfigProfile)
		// The control plane always installs to the smallest disk; it is not
		// configurable through the API.
		assert.Equal(t, db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateSmallest), spec.Nico.DiskSelectionStrategy)
	})

	t.Run("nested infra lands on the stored provider payload", func(t *testing.T) {
		cfg := &api.CreateClusterControlPlaneConfig{
			DesiredCount: 3,
			Infra: &api.InfraControlPlaneProvider{
				MachineTemplate:    tmpl,
				PreKubeadmCommands: []string{"prep"},
			},
		}

		spec := controlPlaneConfigSpecFromCreateAPI(cfg, "1.31.0", true, db.ProviderTypeCAPI)
		assert.Nil(t, spec.Nico)
		require.NotNil(t, spec.Capi)
		require.Len(t, spec.Capi.InfraMachineTemplatePreferences, 1)
		assert.JSONEq(t, `{"kind":"DockerMachineTemplate"}`, string(spec.Capi.InfraMachineTemplatePreferences[0]))
		assert.Empty(t, spec.Capi.SpireAttestationMethod)
		assert.Equal(t, []string{"prep"}, spec.Capi.PreKubeadmCommands)
	})
}

func TestApiCapiControlPlaneProviderFromDB_ExposesOnlyFirstPreference(t *testing.T) {
	cfg := db.ControlPlaneConfig{}
	cfg.SetCapi(db.CAPIControlPlaneProvider{
		InfraMachineTemplatePreferences: []json.RawMessage{
			json.RawMessage(`{"kind":"DockerMachineTemplate","metadata":{"name":"current"}}`),
			json.RawMessage(`{"kind":"DockerMachineTemplate","metadata":{"name":"history"}}`),
		},
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
	})

	got := apiInfraControlPlaneProviderFromDB(cfg.Capi)
	require.NotNil(t, got)
	require.Equal(t, map[string]interface{}{
		"kind":     "DockerMachineTemplate",
		"metadata": map[string]interface{}{"name": "current"},
	}, got.MachineTemplate)
}
