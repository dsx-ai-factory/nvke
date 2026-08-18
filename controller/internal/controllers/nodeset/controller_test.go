// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeset_test

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
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodeset"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

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

func TestUnit_Reconcile(t *testing.T) {
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

	type parameters struct {
		nodeSet db.NodeSet
		cfg     config.Config
		setup   func(*testutil.MockDB, *testutil.MockTx, db.NodeSet)
		verify  func(*testing.T, *testutil.MockDB, *testutil.MockTx)
	}

	tests := map[string]parameters{
		"creates control plane node when control plane controller is disabled": {
			nodeSet: baseNodeSet,
			cfg: config.Config{
				ProvisionControlPlaneWithControlPlaneController: false,
			},
			setup: func(mockDB *testutil.MockDB, mockTx *testutil.MockTx, nodeSet db.NodeSet) {
				mockDB.MockTx = mockTx
				mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
				mockTx.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
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
			},
		},
		"skips control plane creation when control plane controller is enabled": {
			nodeSet: baseNodeSet,
			cfg: config.Config{
				ProvisionControlPlaneWithControlPlaneController: true,
			},
			setup: func(mockDB *testutil.MockDB, mockTx *testutil.MockTx, nodeSet db.NodeSet) {
				mockDB.MockTx = mockTx
				mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
				mockTx.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
				mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(baseNodePool, nil)
			},
			verify: func(t *testing.T, _ *testutil.MockDB, mockTx *testutil.MockTx) {
				t.Helper()

				mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
				mockTx.AssertNotCalled(t, "GetCluster", mock.Anything, clusterID)
				mockTx.AssertNotCalled(t, "ListClusterNodeNames", mock.Anything, clusterID)
				mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
			},
		},
		"skips control plane deletion when control plane controller is enabled": {
			nodeSet: db.NodeSet{
				ID:           nodeSetID,
				NodePoolID:   nodePoolID,
				DesiredCount: 0,
			},
			cfg: config.Config{
				ProvisionControlPlaneWithControlPlaneController: true,
			},
			setup: func(mockDB *testutil.MockDB, mockTx *testutil.MockTx, nodeSet db.NodeSet) {
				mockDB.MockTx = mockTx
				mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
				mockTx.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
				mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(baseNodePool, nil)
			},
			verify: func(t *testing.T, _ *testutil.MockDB, mockTx *testutil.MockTx) {
				t.Helper()

				mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
				mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
				mockTx.AssertNotCalled(t, "DeleteNode", mock.Anything, mock.Anything)
			},
		},
		"skips missing node set": {
			nodeSet: db.NodeSet{ID: nodeSetID},
			setup: func(mockDB *testutil.MockDB, mockTx *testutil.MockTx, nodeSet db.NodeSet) {
				mockDB.MockTx = mockTx
				mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
				mockTx.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(db.NodeSet{}, db.ErrNotFound)
			},
			verify: func(t *testing.T, mockDB *testutil.MockDB, _ *testutil.MockTx) {
				t.Helper()

				mockDB.AssertNotCalled(t, "ListNodeSets", mock.Anything, mock.Anything)
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mockDB := &testutil.MockDB{}
			mockTx := &testutil.MockTx{}
			params.setup(mockDB, mockTx, params.nodeSet)
			params.cfg.ProfilesDir = setupProfilesDir(t, osProfile, configProfile)
			controller := nodeset.NewController(params.cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

			result, err := controller.Reconcile(ctx, dbreconcile.NodeSetKey(params.nodeSet.ID))

			require.NoError(t, err)
			require.Zero(t, result)
			mockDB.AssertExpectations(t)
			mockTx.AssertExpectations(t)
			if params.verify != nil {
				params.verify(t, mockDB, mockTx)
			}
		})
	}
}
