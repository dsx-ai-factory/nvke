// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodepool_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodepool"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_NewController(t *testing.T) {
	npc := nodepool.NewController(config.Config{}, slog.Default(), nil)
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

func TestUnit_NodePoolControllerReconcile(t *testing.T) {
	ctx := context.Background()
	osProfile := "test-os-profile"
	configProfile := "test-config-profile"

	t.Run("node_pool_not_found", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(db.NodePool{}, db.ErrNotFound)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("creates_node_set_when_none", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		clusterID := mustParse("550e8400-e29b-41d4-a716-446655440002")
		subnetID := mustParse("550e8400-e29b-41d4-a716-446655440003")
		newNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440004")
		pool := db.NodePool{
			ID:           poolID,
			ClusterID:    clusterID,
			Name:         "worker",
			DesiredCount: 2,
			Conditions:   db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{},
		})
		nicoNodePoolProvider, _ := pool.NicoProvider()
		newNodeSet := db.NodeSet{
			ID:                   newNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
		}
		newNodeSet.SetNico(nicoNodePoolProvider)
		poolWithCurrent := pool
		poolWithCurrent.CurrentNodeSetID = &newNodeSetID

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
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
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("skips_when_auto_provision_disabled", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		pool := db.NodePool{
			ID:                    poolID,
			ClusterID:             mustParse("550e8400-e29b-41d4-a716-446655440002"),
			Name:                  "worker",
			AutoProvisionDisabled: true,
			Conditions:            db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
		})

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "GetNodeSet", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateNodeSet", mock.Anything, mock.Anything)
	})

	t.Run("skips_when_pool_deleting", func(t *testing.T) {
		poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
		pool := db.NodePool{
			ID:        poolID,
			ClusterID: mustParse("550e8400-e29b-41d4-a716-446655440002"),
			Name:      "worker",
			Conditions: db.NodePoolConditions{
				Deleting: db.Condition{Status: db.ConditionStatusTrue},
			},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
		})

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)

		profilesDir := setupProfilesDir(t, osProfile, configProfile)
		cfg := config.Config{ProfilesDir: profilesDir}
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
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
			DesiredCount:         3,
			CurrentNodeSetID:     &currentNodeSetID,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
			Conditions:           db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{},
		})
		nicoNodePoolProvider, _ := pool.NicoProvider()
		// Current NodeSet matches pool config exactly — no drift expected.
		currentNodeSet := db.NodeSet{
			ID:                   currentNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}
		currentNodeSet.SetNico(nicoNodePoolProvider)

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
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
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
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
			DesiredCount:     2,
			CurrentNodeSetID: &oldNodeSetID,
			Conditions:       db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{newSubnet},
			VpcPrefixIDs:  []uuid.UUID{},
		})
		nicoNodePoolProvider, _ := pool.NicoProvider()
		// Old NodeSet has the previous subnet — this is what triggers drift.
		oldNodeSet := db.NodeSet{
			ID:                   oldNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
		}
		oldNodeSet.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{oldSubnet},
			VpcPrefixIDs:  []uuid.UUID{},
		})
		// New NodeSet matches the pool's current Nico provider.
		newNodeSet := db.NodeSet{
			ID:                   newNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
		}
		newNodeSet.SetNico(nicoNodePoolProvider)
		poolWithNewCurrent := pool
		poolWithNewCurrent.CurrentNodeSetID = &newNodeSetID

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
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
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		mockDB.GetTx = func() db.Tx { return mockTx }
		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
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
			DesiredCount:     2,
			CurrentNodeSetID: &oldNodeSetID,
			Conditions:       db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{newVpcPrefix},
		})
		nicoNodePoolProvider, _ := pool.NicoProvider()
		// Old NodeSet has the previous VPC prefix — this is what triggers drift.
		oldNodeSet := db.NodeSet{
			ID:                   oldNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}
		oldNodeSet.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{oldVpcPrefix},
		})
		// New NodeSet matches the pool's current Nico provider.
		newNodeSet := db.NodeSet{
			ID:                   newNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}
		newNodeSet.SetNico(nicoNodePoolProvider)
		poolWithNewCurrent := pool
		poolWithNewCurrent.CurrentNodeSetID = &newNodeSetID

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
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
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
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
			DesiredCount:         2,
			CurrentNodeSetID:     &currentNodeSetID,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "v1.30.0",
			Conditions:           db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{},
		})
		nicoNodePoolProvider, _ := pool.NicoProvider()
		// Both NodeSets share the same Nico config; only KubernetesVersion differs.
		currentNodeSet := db.NodeSet{
			ID:                   currentNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "v1.30.0",
		}
		currentNodeSet.SetNico(nicoNodePoolProvider)
		oldNodeSet := db.NodeSet{
			ID:                   oldNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         2,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "v1.29.0",
		}
		oldNodeSet.SetNico(nicoNodePoolProvider)
		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
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
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
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
			DesiredCount:         5,
			CurrentNodeSetID:     &currentNodeSetID,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
			Conditions:           db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			OSProfile:     osProfile,
			ConfigProfile: configProfile,
			SubnetIDs:     []uuid.UUID{subnetID},
			VpcPrefixIDs:  []uuid.UUID{},
		})
		nicoNodePoolProvider, _ := pool.NicoProvider()
		// Current NodeSet matches pool config exactly — only DesiredCount needs updating.
		currentNodeSet := db.NodeSet{
			ID:                   currentNodeSetID,
			NodePoolID:           poolID,
			DesiredCount:         0,
			SystemReservedMemory: "16Gi",
			KubernetesVersion:    "",
		}
		currentNodeSet.SetNico(nicoNodePoolProvider)

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
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
		npc := nodepool.NewController(cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

		_, err := npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})
}

func TestUnit_NodePoolControllerReconcileCAPIInfraTemplateChange(t *testing.T) {
	ctx := context.Background()
	poolID := mustParse("550e8400-e29b-41d4-a716-446655440001")
	oldNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440002")
	newNodeSetID := mustParse("550e8400-e29b-41d4-a716-446655440003")
	oldTemplate := json.RawMessage(`{"kind":"OldMachineTemplate"}`)
	newTemplate := json.RawMessage(`{"kind":"NewMachineTemplate"}`)
	pool := db.NodePool{
		ID:               poolID,
		ClusterID:        mustParse("550e8400-e29b-41d4-a716-446655440004"),
		Name:             "worker",
		DesiredCount:     2,
		CurrentNodeSetID: &oldNodeSetID,
		Conditions:       db.NodePoolConditions{},
	}
	pool.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   newTemplate,
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		PreKubeadmCommands:     []string{},
	})
	capiNodePoolProvider, err := pool.CAPIProvider()
	require.NoError(t, err)

	oldNodeSet := db.NodeSet{ID: oldNodeSetID, NodePoolID: poolID}
	oldNodeSet.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   oldTemplate,
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		PreKubeadmCommands:     []string{},
	})
	newNodeSet := db.NodeSet{ID: newNodeSetID, NodePoolID: poolID}
	newNodeSet.SetCapi(capiNodePoolProvider)
	poolWithNewCurrent := pool
	poolWithNewCurrent.CurrentNodeSetID = &newNodeSetID

	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetNodePoolForUpdate", mock.Anything, poolID).Return(pool, nil)
	mockTx.On("GetNodeSet", mock.Anything, oldNodeSetID).Return(oldNodeSet, nil)
	mockTx.On("CreateNodeSet", mock.Anything, mock.MatchedBy(func(ns *db.NodeSet) bool {
		return ns.NodePoolID == poolID && string(ns.CapiInfraMachineTemplate) == string(newTemplate)
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

	profilesDir := setupProfilesDir(t, "test-os-profile", "test-config-profile")
	npc := nodepool.NewController(config.Config{ProfilesDir: profilesDir}, slog.Default(), dbwrapper.New(mockDB, slog.Default()))
	mockDB.GetTx = func() db.Tx { return mockTx }

	_, err = npc.Reconcile(ctx, dbreconcile.NodePoolKey(poolID))
	require.NoError(t, err)
	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}
