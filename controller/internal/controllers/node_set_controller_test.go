// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func setupProfilesDir(t *testing.T, osProfile, configProfile string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "osProfiles", osProfile), 0o755))
	configPath := filepath.Join(dir, "configProfiles", configProfile+".yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(""), 0o644))
	return dir
}

func TestNodeSetController_ControlPlaneProvisioningGate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	site := db.Site{ID: uuid.New()}
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	instanceTypeID := uuid.New()
	osProfile := "test-os-profile"
	configProfile := "test-config-profile"

	baseNodePool := db.NodePool{
		ID:                    nodePoolID,
		ClusterID:             clusterID,
		ControlPlane:          true,
		NodeNamePrefix:        "cp",
		CurrentNodeSetID:      &nodeSetID,
		AutoProvisionDisabled: false,
		Conditions:            db.NodePoolConditions{},
	}
	baseNodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: instanceTypeID,
	})
	baseNodeSet := db.NodeSet{
		ID:           nodeSetID,
		NodePoolID:   nodePoolID,
		DesiredCount: 1,
	}
	baseCluster := db.Cluster{
		ID:           clusterID,
		Name:         "test-cluster",
		SiteID:       site.ID,
		OrgID:        "test-org",
		VpcID:        uuid.New(),
		ProviderType: db.ProviderTypeNico,
	}

	t.Run("flag_off_node_set_controller_creates_control_plane_node", func(t *testing.T) {
		t.Parallel()

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodeSets", mock.Anything, mock.MatchedBy(func(f db.NodeSetFilters) bool {
			return f.SiteID != nil && *f.SiteID == site.ID
		})).Return([]db.NodeSet{baseNodeSet}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(baseNodePool, nil)
		mockTx.On("ListNodes", mock.Anything, db.NodeFilters{NodeSetID: &nodeSetID}).Return([]db.Node{}, nil)
		mockTx.On("GetCluster", mock.Anything, clusterID).Return(baseCluster, nil)
		mockTx.On("ListClusterNodeNames", mock.Anything, clusterID).Return([]string{"existing-cp"}, nil)
		mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
			return node.ClusterID == clusterID &&
				node.NodePoolID == nodePoolID &&
				node.NodeSetID == nodeSetID &&
				node.ControlPlane &&
				node.InstanceTypeID == instanceTypeID
		})).Return(db.Node{}, nil).Once()

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{
			ProfilesDir: profilesDir,
			ProvisionControlPlaneWithControlPlaneController: false,
		}
		controller := controllers.NewNodeSetController(cfg, slog.Default(), mockDB)
		err := controller.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("flag_on_node_set_controller_skips_control_plane_creation", func(t *testing.T) {
		t.Parallel()

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodeSets", mock.Anything, mock.MatchedBy(func(f db.NodeSetFilters) bool {
			return f.SiteID != nil && *f.SiteID == site.ID
		})).Return([]db.NodeSet{baseNodeSet}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(baseNodePool, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{
			ProfilesDir: profilesDir,
			ProvisionControlPlaneWithControlPlaneController: true,
		}
		controller := controllers.NewNodeSetController(cfg, slog.Default(), mockDB)
		err := controller.Reconcile(ctx, site)
		require.NoError(t, err)

		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		// The control-plane node set is skipped entirely before any counting work,
		// so the node set controller never lists nodes or creates pending records.
		mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "GetCluster", mock.Anything, clusterID)
		mockTx.AssertNotCalled(t, "ListClusterNodeNames", mock.Anything, clusterID)
		mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
	})

	t.Run("flag_on_node_set_controller_skips_control_plane_deletion", func(t *testing.T) {
		t.Parallel()

		// Surplus scenario: desired count 0 while a node-set-tied pending node still
		// exists (deficit < 0). With the flag on, the control-plane controller fully
		// owns the control-plane node lifecycle, so the NodeSet controller must skip
		// the set entirely and must NOT delete the pending node.
		surplusNodeSet := db.NodeSet{
			ID:           nodeSetID,
			NodePoolID:   nodePoolID,
			DesiredCount: 0,
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodeSets", mock.Anything, mock.MatchedBy(func(f db.NodeSetFilters) bool {
			return f.SiteID != nil && *f.SiteID == site.ID
		})).Return([]db.NodeSet{surplusNodeSet}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(baseNodePool, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{
			ProfilesDir: profilesDir,
			ProvisionControlPlaneWithControlPlaneController: true,
		}
		controller := controllers.NewNodeSetController(cfg, slog.Default(), mockDB)
		err := controller.Reconcile(ctx, site)
		require.NoError(t, err)

		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		// Fully skipped: the controller neither lists/creates nor deletes any
		// node-set-tied pending control-plane nodes.
		mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "DeleteNode", mock.Anything, mock.Anything)
	})
}
