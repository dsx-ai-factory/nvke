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

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
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
		req.ControlPlaneConfig = validForgeControlPlaneConfig()

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

func TestApplyClusterLevelControlPlaneSpecUpdate_ScaleDown(t *testing.T) {
	svc := clusterService{useControlPlaneController: true}
	ctx := context.Background()

	newCAPICluster := func(current int) *db.Cluster {
		return &db.Cluster{
			ID:     uuid.New(),
			SiteID: uuid.Nil, // CAPI cluster avoids Forge networking lookups.
			ControlPlaneConfig: db.ControlPlaneConfig{
				DesiredCount: current,
			},
		}
	}

	t.Run("allows scale-down below current", func(t *testing.T) {
		cluster := newCAPICluster(3)
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 2
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.NoError(t, err)
		require.Equal(t, 2, cluster.ControlPlaneConfig.DesiredCount, "scale-down must be accepted and persisted")
	})

	t.Run("allows equal desiredCount", func(t *testing.T) {
		cluster := newCAPICluster(3)
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 3
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.NoError(t, err)
		require.Equal(t, 3, cluster.ControlPlaneConfig.DesiredCount)
	})

	t.Run("allows scale-up", func(t *testing.T) {
		cluster := newCAPICluster(3)
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 5
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.NoError(t, err)
		require.Equal(t, 5, cluster.ControlPlaneConfig.DesiredCount)
	})

	t.Run("rejects desiredCount below 1", func(t *testing.T) {
		cluster := newCAPICluster(3)
		spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
		desired := 0
		err := svc.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		}, &spec)
		require.Error(t, err)
		require.Contains(t, err.Error(), "desiredCount must be at least 1")
	})
}

func TestCreateNodePool_RejectsControlPlanePoolWhenControllerEnabled(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:     clusterID,
		OrgID:  "test-org",
		SiteID: uuid.New(),
	}, nil)

	service := NewNodePoolService(slog.Default(), mockDB, config.TeleportConfig{}, true)

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
				ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
					InstanceTypePreference: []uuid.UUID{instanceType},
					OSProfile:              "ubuntu",
					ConfigProfile:          "default",
				},
			},
			Conditions: db.ClusterConditions{
				ControlPlaneUpgrading: db.NewCondition(true, "upgrading to 1.29.0"),
				CPNodesRecycling:      db.NewCondition(false, "not recycling"),
			},
		}
		cluster.ControlPlaneConfig.ControlPlaneConfigSpec.SyncProviderFields(db.ProviderTypeNico)
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
			ID:     uuid.New(),
			OrgID:  "test-org",
			SiteID: uuid.New(), // Forge cluster.
			ControlPlaneConfig: db.ControlPlaneConfig{
				DesiredCount: 3,
				ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
					InstanceTypePreference: []uuid.UUID{uuid.New()},
					OSProfile:              "ubuntu",
					ConfigProfile:          "default",
				},
			},
		}
		cluster.SyncProviderFields()
		cluster.ControlPlaneConfig.ControlPlaneConfigSpec.SyncProviderFields(db.ProviderTypeNico)
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
				MachineTemplate:        tmpl,
				SpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
				PreKubeadmCommands:     []string{"prep"},
			},
		}

		spec := controlPlaneConfigSpecFromCreateAPI(cfg, "1.31.0", true, db.ProviderTypeCAPI)
		assert.Nil(t, spec.Nico)
		require.NotNil(t, spec.Capi)
		require.Len(t, spec.Capi.InfraMachineTemplatePreferences, 1)
		assert.JSONEq(t, `{"kind":"DockerMachineTemplate"}`, string(spec.Capi.InfraMachineTemplatePreferences[0]))
		assert.Equal(t, db.SpireAttestationMethodTPM, spec.Capi.SpireAttestationMethod)
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
