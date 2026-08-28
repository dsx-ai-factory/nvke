// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
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
		DesiredCount:           3,
		InstanceTypePreference: []uuid.UUID{uuid.New()},
		OSProfile:              "ubuntu",
		ConfigProfile:          "default",
	}
}

func validForgeCreateClusterRequest() *api.CreateClusterRequest {
	return &api.CreateClusterRequest{
		Name:                          "runtime-dgxc-k8s-forge-cp",
		OrgID:                         "test-org",
		SiteID:                        uuid.New(),
		VpcID:                         uuid.New(),
		CniPlugin:                     "calico",
		ControlPlaneKubernetesVersion: "1.29.0",
	}
}

func TestValidateCreateClusterRequestFields_ControlPlaneControllerFlag(t *testing.T) {
	t.Run("flag OFF rejects cluster-level controlPlaneConfig", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: false}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()
		// Provide a control-plane node pool so the legacy "must have a CP node" rule passes.
		req.NodePools = &[]api.NodePoolCreateConfig{{
			Name:             "cp",
			NodeNamePrefix:   "cp",
			SubnetIDs:        []uuid.UUID{uuid.New()},
			InstanceTypeID:   uuid.New(),
			OSProfile:        "ubuntu",
			ConfigProfile:    "default",
			ControlPlane:     true,
			DesiredNodeCount: 1,
		}}

		err := svc.validateCreateClusterRequestFields(req)
		require.ErrorIs(t, err, errControlPlaneConfigNotEnabled)
	})

	t.Run("flag ON rejects control-plane node pools", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()
		req.NodePools = &[]api.NodePoolCreateConfig{{
			Name:             "cp",
			NodeNamePrefix:   "cp",
			SubnetIDs:        []uuid.UUID{uuid.New()},
			InstanceTypeID:   uuid.New(),
			OSProfile:        "ubuntu",
			ConfigProfile:    "default",
			ControlPlane:     true,
			DesiredNodeCount: 1,
		}}

		err := svc.validateCreateClusterRequestFields(req)
		require.ErrorIs(t, err, errControlPlaneNodePoolNotAllowed)
	})

	t.Run("flag ON requires controlPlaneConfig", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()

		err := svc.validateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "controlPlaneConfig is required")
	})

	t.Run("flag ON rejects desiredCount below 1", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		cp := validForgeControlPlaneConfig()
		cp.DesiredCount = 0
		req.ControlPlaneConfig = cp

		err := svc.validateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "desiredCount must be at least 1")
	})

	t.Run("flag ON accepts a valid Forge cluster-level control plane with no node pools", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()

		err := svc.validateCreateClusterRequestFields(req)
		require.NoError(t, err)
	})

	t.Run("flag ON rejects a nil instance type ID in instanceTypePreference", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := validForgeCreateClusterRequest()
		cp := validForgeControlPlaneConfig()
		// A nil UUID passes the length check but would be silently dropped by
		// dedupeUUIDsPreserveOrder, so it must be rejected up front.
		cp.InstanceTypePreference = []uuid.UUID{uuid.New(), uuid.Nil}
		req.ControlPlaneConfig = cp

		err := svc.validateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid nil instance type ID")
	})

	t.Run("flag ON rejects Forge-only field on a CAPI cluster control plane", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		req := &api.CreateClusterRequest{
			Name:                          "capi-cluster",
			SiteID:                        uuid.Nil,
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{
				DesiredCount: 3,
				OSProfile:    "ubuntu",
			},
		}

		err := svc.validateCreateClusterRequestFields(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "osProfile is not supported for CAPI clusters")
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

	service := NewNodePoolService(slog.Default(), mockDB, nil, config.TeleportConfig{}, true)

	resp, err := service.CreateNodePool(context.Background(), api.CreateNodePoolRequestObject{
		Body: &api.CreateNodePoolRequest{
			ClusterID:        clusterID,
			Name:             "cp",
			NodeNamePrefix:   "cp",
			ConfigProfile:    "default",
			OSProfile:        "ubuntu",
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
		return db.Cluster{
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
		require.Equal(t, "ubuntu", out.Config.OSProfile)
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
		return &db.Cluster{
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
	}

	// tx is never reached: validation fails before the config row is persisted.
	t.Run("clearing osProfile is rejected", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		cluster := newForgeCluster()
		empty := ""
		err := svc.applyClusterControlPlaneConfigUpdate(ctx, nil, cluster, &api.UpdateClusterControlPlaneConfig{
			OSProfile: &empty,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "osProfile is required")
	})

	t.Run("clearing configProfile is rejected", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		cluster := newForgeCluster()
		empty := ""
		err := svc.applyClusterControlPlaneConfigUpdate(ctx, nil, cluster, &api.UpdateClusterControlPlaneConfig{
			ConfigProfile: &empty,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "configProfile is required")
	})

	// instanceTypePreference is merged in the parent (not applyClusterLevelControlPlaneSpecUpdate),
	// so this exercises the parent-level merged-spec validation.
	t.Run("clearing instanceTypePreference is rejected", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		cluster := newForgeCluster()
		emptyPrefs := []uuid.UUID{}
		err := svc.applyClusterControlPlaneConfigUpdate(ctx, nil, cluster, &api.UpdateClusterControlPlaneConfig{
			InstanceTypePreference: &emptyPrefs,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "instanceTypePreference must contain at least one")
	})

	t.Run("nil instance type ID in instanceTypePreference is rejected", func(t *testing.T) {
		svc := clusterService{useControlPlaneController: true}
		cluster := newForgeCluster()
		prefs := []uuid.UUID{uuid.New(), uuid.Nil}
		err := svc.applyClusterControlPlaneConfigUpdate(ctx, nil, cluster, &api.UpdateClusterControlPlaneConfig{
			InstanceTypePreference: &prefs,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid nil instance type ID")
	})
}
