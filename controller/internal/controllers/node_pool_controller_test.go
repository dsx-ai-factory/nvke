// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestNewNodePoolController(t *testing.T) {
	npc := controllers.NewNodePoolController(config.Config{}, slog.Default(), nil)
	assert.NotNil(t, npc)
}

// mustParse returns uuid.MustParse(s). Use for static UUIDs in tests.
func mustParse(s string) uuid.UUID {
	return uuid.MustParse(s)
}

// setupProfilesDir creates a temp dir with osProfiles and configProfiles so
// isOSProfileDeprecated / isConfigProfileDeprecated return false for test profile names.
func setupProfilesDir(t *testing.T, osProfile, configProfile string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "osProfiles", osProfile), 0o755))
	configPath := filepath.Join(dir, "configProfiles", configProfile+".yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(""), 0o644))
	return dir
}

func TestNodePoolController_Reconcile(t *testing.T) {
	ctx := context.Background()
	siteID := mustParse("550e8400-e29b-41d4-a716-446655440000")
	site := db.Site{ID: siteID}
	osProfile := "test-os-profile"
	configProfile := "test-config-profile"

	t.Run("no_node_pools", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("ListNodePools", mock.Anything, mock.MatchedBy(func(f db.NodePoolFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID
		})).Return([]db.NodePool{}, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
	})

	t.Run("creates_node_set_when_none", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		clusterID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		subnetID := mustParse("550e8400-e29b-41d4-a716-446655440003")
		newNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440004")
		pool := db.NodePool{
			ID:            poolID,
			ClusterID:     clusterID,
			Name:          "worker",
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			DesiredCount:  2,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{},
			Conditions:    db.NodePoolConditions{},
		}
		newNodeSet := db.NodeSet{
			ID:                   newNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            pool.SubnetIDs,
			VpcPrefixIDs:         pool.VpcPrefixIDs,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
		}
		poolWithCurrent := pool
		poolWithCurrent.CurrentNodeSetID = &newNodeSetID

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("CreateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.NodePoolID == poolID && ns.OSProfile == osProfile && ns.ConfigProfile == configProfile
		})).Return(newNodeSet, nil)
		mockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(np *db.NodePool) bool {
			return np.ID == poolID && np.CurrentNodeSetID != nil && *np.CurrentNodeSetID == newNodeSetID
		})).Return(poolWithCurrent, nil)
		mockTx.On("ListNodeSets", mock.Anything, mock.MatchedBy(func(f db.NodeSetFilters) bool {
			return f.NodePoolID != nil && *f.NodePoolID == poolID
		})).Return([]db.NodeSet{newNodeSet}, nil).Once()
		// ListNodes is only called for non-current node sets; with a single (current) set it is not called
		mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil)
		mockTx.On("UpdateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.ID == newNodeSetID && ns.DesiredCount == 2
		})).Return(newNodeSet, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("skips_when_auto_provision_disabled", func(t *testing.T) {
		pool := db.NodePool{
			ID:                    mustParse("550e8400-e29b-41d4-a716-446655440001"),
			ClusterID:             mustParse("550e8400-e29b-41d4-a716-446655440002"),
			Name:                  "worker",
			OSProfile:             osProfile,
			ConfigProfile:         configProfile,
			AutoProvisionDisabled: true,
			Conditions:            db.NodePoolConditions{},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "GetNodeSet", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateNodeSet", mock.Anything, mock.Anything)
	})

	t.Run("skips_when_pool_deleting", func(t *testing.T) {
		pool := db.NodePool{
			ID:            mustParse("550e8400-e29b-41d4-a716-446655440001"),
			ClusterID:     mustParse("550e8400-e29b-41d4-a716-446655440002"),
			Name:          "worker",
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			Conditions: db.NodePoolConditions{
				Deleting: db.Condition{Status: db.ConditionStatusTrue},
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "GetNodeSet", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateNodeSet", mock.Anything, mock.Anything)
	})

	t.Run("no_new_node_set_when_config_unchanged", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		currentNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		subnetID := mustParse("550e8400-e29b-41d4-a716-446655440003")
		pool := db.NodePool{
			ID:                   poolID,
			ClusterID:            mustParse("550e8400-e29b-41d4-a716-446655440004"),
			Name:                 "worker",
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			DesiredCount:         3,
			CurrentNodeSetID:     &currentNodeSetID,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
			Conditions:           db.NodePoolConditions{},
		}
		currentNodeSet := db.NodeSet{
			ID:                   currentNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodeSet", mock.Anything, currentNodeSetID).Return(currentNodeSet, nil)
		mockTx.On("ListNodeSets", mock.Anything, mock.Anything).Return([]db.NodeSet{currentNodeSet}, nil).Once()
		// ListNodes is only called for non-current node sets; with a single (current) set it is not called
		updatedSet := currentNodeSet
		updatedSet.DesiredCount = 3
		mockTx.On("UpdateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.ID == currentNodeSetID && ns.DesiredCount == 3
		})).Return(updatedSet, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "CreateNodeSet", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
	})

	t.Run("creates_new_node_set_when_subnet_changed", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		oldNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		oldSubnet := mustParse("550e8400-e29b-41d4-a716-446655440003")
		newSubnet := mustParse("550e8400-e29b-41d4-a716-446655440004")
		newNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440005")
		pool := db.NodePool{
			ID:               poolID,
			ClusterID:        mustParse("550e8400-e29b-41d4-a716-446655440006"),
			Name:             "worker",
			OSProfile:        osProfile,
			ConfigProfile:    configProfile,
			DesiredCount:     2,
			CurrentNodeSetID: &oldNodeSetID,
			SubnetIDs:        []uuid.UUID{newSubnet},
			VpcPrefixIDs:     []uuid.UUID{},
			Conditions:       db.NodePoolConditions{},
		}
		oldNodeSet := db.NodeSet{
			ID:                   oldNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{oldSubnet},
			VpcPrefixIDs:         []uuid.UUID{},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
		}
		newNodeSet := db.NodeSet{
			ID:                   newNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{newSubnet},
			VpcPrefixIDs:         []uuid.UUID{},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
		}
		poolWithNewCurrent := pool
		poolWithNewCurrent.CurrentNodeSetID = &newNodeSetID

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodeSet", mock.Anything, oldNodeSetID).Return(oldNodeSet, nil)
		mockTx.On("CreateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.NodePoolID == poolID && len(ns.SubnetIDs) == 1 && ns.SubnetIDs[0] == newSubnet
		})).Return(newNodeSet, nil)
		mockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(np *db.NodePool) bool {
			return np.CurrentNodeSetID != nil && *np.CurrentNodeSetID == newNodeSetID
		})).Return(poolWithNewCurrent, nil)
		mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil)
		mockTx.On("ListNodeSets", mock.Anything, mock.Anything).Return([]db.NodeSet{oldNodeSet, newNodeSet}, nil).Once()
		mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{}, nil)
		mockTx.On("DeleteNodeSet", mock.Anything, oldNodeSetID).Return(nil)
		updatedNewSet := newNodeSet
		updatedNewSet.DesiredCount = 2
		mockTx.On("UpdateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.ID == newNodeSetID && ns.DesiredCount == 2
		})).Return(updatedNewSet, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		// Use GetTx so the callback always receives mockTx (avoids nil receiver when MockTx field is not reliable).
		mockDB.GetTx = func() db.Tx { return mockTx }
		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("creates_new_node_set_when_vpc_prefix_ids_changed", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		oldNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		oldVpcPrefix := mustParse("550e8400-e29b-41d4-a716-446655440003")
		newVpcPrefix := mustParse("550e8400-e29b-41d4-a716-446655440004")
		subnetID := mustParse("550e8400-e29b-41d4-a716-446655440005")
		newNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440006")
		pool := db.NodePool{
			ID:               poolID,
			ClusterID:        mustParse("550e8400-e29b-41d4-a716-446655440007"),
			Name:             "worker",
			OSProfile:        osProfile,
			ConfigProfile:    configProfile,
			DesiredCount:     2,
			CurrentNodeSetID: &oldNodeSetID,
			SubnetIDs:        []uuid.UUID{subnetID},
			VpcPrefixIDs:     []uuid.UUID{newVpcPrefix},
			Conditions:       db.NodePoolConditions{},
		}
		oldNodeSet := db.NodeSet{
			ID:                   oldNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{oldVpcPrefix},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}
		newNodeSet := db.NodeSet{
			ID:                   newNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{newVpcPrefix},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}
		poolWithNewCurrent := pool
		poolWithNewCurrent.CurrentNodeSetID = &newNodeSetID

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodeSet", mock.Anything, oldNodeSetID).Return(oldNodeSet, nil)
		mockTx.On("CreateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.NodePoolID == poolID && len(ns.VpcPrefixIDs) == 1 && ns.VpcPrefixIDs[0] == newVpcPrefix
		})).Return(newNodeSet, nil)
		mockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(np *db.NodePool) bool {
			return np.CurrentNodeSetID != nil && *np.CurrentNodeSetID == newNodeSetID
		})).Return(poolWithNewCurrent, nil)
		mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil)
		mockTx.On("ListNodeSets", mock.Anything, mock.Anything).Return([]db.NodeSet{oldNodeSet, newNodeSet}, nil).Once()
		mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{}, nil)
		mockTx.On("DeleteNodeSet", mock.Anything, oldNodeSetID).Return(nil)
		updatedNewSet := newNodeSet
		updatedNewSet.DesiredCount = 2
		mockTx.On("UpdateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.ID == newNodeSetID && ns.DesiredCount == 2
		})).Return(updatedNewSet, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("auto_deprecates_node_set_on_k8s_version_change", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		currentNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		oldNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440003")
		subnetID := mustParse("550e8400-e29b-41d4-a716-446655440004")
		pool := db.NodePool{
			ID:                   poolID,
			ClusterID:            mustParse("550e8400-e29b-41d4-a716-446655440005"),
			Name:                 "worker",
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			DesiredCount:         2,
			CurrentNodeSetID:     &currentNodeSetID,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "v1.30.0",
			Conditions:           db.NodePoolConditions{},
		}
		currentNodeSet := db.NodeSet{
			ID:                   currentNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "v1.30.0",
		}
		// oldNodeSet has an outdated k8s version and is non-current.
		oldNodeSet := db.NodeSet{
			ID:                   oldNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			DesiredCount:         2,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "v1.29.0",
		}
		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodeSet", mock.Anything, currentNodeSetID).Return(currentNodeSet, nil)
		mockTx.On("ListNodeSets", mock.Anything, mock.Anything).Return([]db.NodeSet{currentNodeSet, oldNodeSet}, nil).Once()
		mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{}, nil)
		mockTx.On("DeleteNodeSet", mock.Anything, oldNodeSetID).Return(nil)
		updatedCurrentSet := currentNodeSet
		updatedCurrentSet.DesiredCount = 2
		mockTx.On("UpdateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.ID == currentNodeSetID && ns.DesiredCount == 2
		})).Return(updatedCurrentSet, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("desired_count_on_current_node_set", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		currentNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		subnetID := mustParse("550e8400-e29b-41d4-a716-446655440003")
		pool := db.NodePool{
			ID:                   poolID,
			ClusterID:            mustParse("550e8400-e29b-41d4-a716-446655440004"),
			Name:                 "worker",
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			DesiredCount:         5,
			CurrentNodeSetID:     &currentNodeSetID,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
			Conditions:           db.NodePoolConditions{},
		}
		currentNodeSet := db.NodeSet{
			ID:                   currentNodeSetID,
			NodePoolID:           poolID,
			OSProfile:            osProfile,
			ConfigProfile:        configProfile,
			SubnetIDs:            []uuid.UUID{subnetID},
			VpcPrefixIDs:         []uuid.UUID{},
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{pool}, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodeSet", mock.Anything, currentNodeSetID).Return(currentNodeSet, nil)
		mockTx.On("ListNodeSets", mock.Anything, mock.Anything).Return([]db.NodeSet{currentNodeSet}, nil).Once()
		// ListNodes is only called for non-current node sets; with a single (current) set it is not called
		updatedSet := currentNodeSet
		updatedSet.DesiredCount = 5
		mockTx.On("UpdateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
			return ns.ID == currentNodeSetID && ns.DesiredCount == 5
		})).Return(updatedSet, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := controllers.NewNodePoolController(cfg, slog.Default(), mockDB)

		err := npc.Reconcile(ctx, site)
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})
}
