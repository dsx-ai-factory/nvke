// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestListClusters(t *testing.T) {
	siteID1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	siteID2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	tests := []struct {
		name          string
		seedClusters  []db.Cluster
		filters       db.ClusterFilters
		expectedNames []string
		expectedErr   error
	}{
		{
			name: "no filters returns all clusters",
			seedClusters: []db.Cluster{
				newValidCluster(withName("cluster-a"), withSiteID(siteID1)),
				newValidCluster(withName("cluster-b"), withSiteID(siteID2)),
			},
			filters:       db.ClusterFilters{},
			expectedNames: []string{"cluster-a", "cluster-b"},
		},
		{
			name: "filter by site ID returns only matching clusters",
			seedClusters: []db.Cluster{
				newValidCluster(withName("cluster-site1-a"), withSiteID(siteID1)),
				newValidCluster(withName("cluster-site1-b"), withSiteID(siteID1)),
				newValidCluster(withName("cluster-site2"), withSiteID(siteID2)),
			},
			filters:       db.ClusterFilters{SiteID: &siteID1},
			expectedNames: []string{"cluster-site1-a", "cluster-site1-b"},
		},
		{
			name: "filter by site ID returns empty when no matches",
			seedClusters: []db.Cluster{
				newValidCluster(withName("cluster-a"), withSiteID(siteID1)),
			},
			filters:       db.ClusterFilters{SiteID: &siteID2},
			expectedNames: []string{},
		},
		{
			name: "filter by name returns only matching cluster",
			seedClusters: []db.Cluster{
				newValidCluster(withName("target-cluster")),
				newValidCluster(withName("other-cluster")),
			},
			filters:       db.ClusterFilters{Name: testutil.PtrTo("target-cluster")},
			expectedNames: []string{"target-cluster"},
		},
		{
			name: "filter by both name and site ID",
			seedClusters: []db.Cluster{
				newValidCluster(withName("shared-name"), withSiteID(siteID1)),
				newValidCluster(withName("shared-name"), withSiteID(siteID2)),
			},
			filters:       db.ClusterFilters{Name: testutil.PtrTo("shared-name"), SiteID: &siteID1},
			expectedNames: []string{"shared-name"},
		},
		{
			name:          "empty database returns empty list",
			seedClusters:  []db.Cluster{},
			filters:       db.ClusterFilters{},
			expectedNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil))

			// Seed test data
			if len(tt.seedClusters) > 0 {
				_, err := bunDB.NewInsert().Model(&tt.seedClusters).Exec(ctx)
				require.NoError(t, err, "failed to seed test data")
			}

			// Create the DB implementation
			dbImpl := db.NewDB(bunDB)

			// Execute ListClusters
			clusters, err := dbImpl.ListClusters(ctx, tt.filters)

			if tt.expectedErr != nil {
				assert.ErrorIs(t, err, tt.expectedErr)
				return
			}

			require.NoError(t, err)
			assert.Len(t, clusters, len(tt.expectedNames))

			// Verify returned cluster names
			actualNames := make([]string, len(clusters))
			for i, c := range clusters {
				actualNames[i] = c.Name
			}
			assert.ElementsMatch(t, tt.expectedNames, actualNames)
		})
	}
}

func newValidCluster(opts ...func(*db.Cluster)) db.Cluster {
	now := time.Now()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              fmt.Sprintf("cluster-%s", uuid.New().String()),
		OrgID:             "org1",
		SiteID:            uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		VpcID:             uuid.New(),
		KubernetesVersion: "1.29",
		Conditions:        db.ClusterConditions{},
		CreatedAt:         now,
		UpdatedAt:         now,
		EtcdEncrypted:     true,
	}

	for _, opt := range opts {
		opt(&cluster)
	}
	return cluster
}

func withName(name string) func(*db.Cluster) {
	return func(c *db.Cluster) { c.Name = name }
}

func withSiteID(siteID uuid.UUID) func(*db.Cluster) {
	return func(c *db.Cluster) { c.SiteID = siteID }
}

func TestControlPlaneConfigFromNodePool(t *testing.T) {
	t.Parallel()

	inst := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	sub := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	np := &db.NodePool{
		ControlPlane:                true,
		DesiredCount:                3,
		InstanceTypeID:              inst,
		SubnetIDs:                   []uuid.UUID{sub},
		VpcPrefixIDs:                []uuid.UUID{},
		OSProfile:                   "nke-test-os",
		ConfigProfile:               "nke-test-cfg",
		SSHKeyGroupIDs:              []uuid.UUID{},
		SystemReservedMemory:        "8Gi",
		NodeNamePrefix:              "cp-",
		NodeLabels:                  []string{"role=cp"},
		TeleportEnabled:             true,
		ProvisioningMonitorDisabled: true,
		DiskSelectionStrategy:       db.DiskSelectionStrategyLargest,
		MaxSurge:                    1,
		KubernetesVersion:           "1.31.0",
		CapiSpireAttestationMethod:  db.SpireAttestationMethodJoinToken,
		CapiBootstrapFiles:          []db.CloudInitFile{{Path: "/x", Content: "y"}},
		CapiPreKubeadmCommands:      []string{"echo hi"},
		CapiInfraMachineTemplate:    json.RawMessage(`{"kind":"Fake"}`),
	}

	cfg := db.ControlPlaneConfigFromNodePool(np)
	require.NotNil(t, cfg)
	assert.Equal(t, 3, cfg.DesiredCount)
	assert.Equal(t, []uuid.UUID{inst}, cfg.InstanceTypePreference)
	require.Len(t, cfg.InfraMachineTemplatePreferences, 1)
	assert.JSONEq(t, `{"kind":"Fake"}`, string(cfg.InfraMachineTemplatePreferences[0]))
	assert.Equal(t, []uuid.UUID{sub}, cfg.SubnetIDs)
	assert.Equal(t, "nke-test-os", cfg.OSProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, cfg.DiskSelectionStrategy)
	assert.Equal(t, "nke-test-cfg", cfg.ConfigProfile)
	assert.Equal(t, []string{"role=cp"}, cfg.NodeLabels)
	assert.True(t, cfg.TeleportEnabled)
	assert.True(t, cfg.ProvisioningMonitorDisabled)
	assert.Equal(t, 0, cfg.MaxSurge)
	assert.Equal(t, "1.31.0", cfg.KubernetesVersion)
	assert.Equal(t, db.SpireAttestationMethodJoinToken, cfg.CapiSpireAttestationMethod)
	assert.Len(t, cfg.CapiBootstrapFiles, 1)
	assert.Equal(t, []string{"echo hi"}, cfg.CapiPreKubeadmCommands)

	assert.Nil(t, db.ControlPlaneConfigFromNodePool(nil))

	worker := *np
	worker.ControlPlane = false
	assert.Nil(t, db.ControlPlaneConfigFromNodePool(&worker))
}

func TestMergeControlPlaneConfigFromNodePools(t *testing.T) {
	t.Parallel()

	instA := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-aaaaaaaaaaaa")
	instB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	sub1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	sub2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	t1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	p1 := db.NodePool{
		ControlPlane:                true,
		DesiredCount:                3,
		InstanceTypeID:              instA,
		Name:                        "gpu-control-plane",
		CapiInfraMachineTemplate:    json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate","metadata":{"name":"cp-a"}}`),
		SubnetIDs:                   []uuid.UUID{sub1},
		VpcPrefixIDs:                []uuid.UUID{},
		OSProfile:                   "os-a",
		ConfigProfile:               "cfg-a",
		NodeNamePrefix:              "cp-",
		NodeLabels:                  []string{"a=1"},
		TeleportEnabled:             false,
		ProvisioningMonitorDisabled: true,
		DiskSelectionStrategy:       db.DiskSelectionStrategySmallest,
		MaxSurge:                    1,
		KubernetesVersion:           "1.31.0",
		CreatedAt:                   t1,
	}
	p2 := db.NodePool{
		ControlPlane:             true,
		DesiredCount:             2,
		InstanceTypeID:           instB,
		Name:                     "cpu-control-plane",
		CapiInfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate","metadata":{"name":"cp-b"}}`),
		SubnetIDs:                []uuid.UUID{sub1, sub2},
		VpcPrefixIDs:             []uuid.UUID{},
		OSProfile:                "os-b",
		ConfigProfile:            "cfg-b",
		NodeNamePrefix:           "cp2-",
		NodeLabels:               []string{"b=2"},
		TeleportEnabled:          true,
		DiskSelectionStrategy:    db.DiskSelectionStrategyLargest,
		MaxSurge:                 0,
		KubernetesVersion:        "1.31.0",
		CreatedAt:                t2,
	}

	// Newest first (required contract), newest pool provides scalar defaults.
	cfg := db.MergeControlPlaneConfigFromNodePools([]db.NodePool{p2, p1})
	require.NotNil(t, cfg)
	assert.Equal(t, 5, cfg.DesiredCount)
	assert.Equal(t, []uuid.UUID{instA, instB}, cfg.InstanceTypePreference)
	require.Len(t, cfg.InfraMachineTemplatePreferences, 2)
	assert.JSONEq(t, `{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate","metadata":{"name":"cp-b"}}`, string(cfg.InfraMachineTemplatePreferences[0]))
	assert.JSONEq(t, `{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate","metadata":{"name":"cp-a"}}`, string(cfg.InfraMachineTemplatePreferences[1]))
	assert.Equal(t, []uuid.UUID{sub1, sub2}, cfg.SubnetIDs)
	assert.Equal(t, "os-b", cfg.OSProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, cfg.DiskSelectionStrategy)
	assert.Equal(t, "cfg-b", cfg.ConfigProfile)
	assert.ElementsMatch(t, []string{"a=1", "b=2"}, cfg.NodeLabels)
	assert.True(t, cfg.TeleportEnabled)
	assert.True(t, cfg.ProvisioningMonitorDisabled)
	assert.Equal(t, 0, cfg.MaxSurge)

	assert.Nil(t, db.MergeControlPlaneConfigFromNodePools(nil))
	assert.Nil(t, db.MergeControlPlaneConfigFromNodePools([]db.NodePool{}))
}

func TestMergeControlPlaneConfigFromNodePools_InstanceTypeOrderOldestFirst(t *testing.T) {
	t.Parallel()

	instCPU := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	instStorage := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	instGPU := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003")
	instOther := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000004")
	now := time.Now()

	// Newest first (required contract); resulting instance preference must be oldest-first.
	pools := []db.NodePool{
		{ControlPlane: true, Name: "mixed", InstanceTypeID: instOther, DesiredCount: 1, OSProfile: "os", ConfigProfile: "cfg", CreatedAt: now.Add(3 * time.Second)},
		{ControlPlane: true, Name: "cpu-cp", InstanceTypeID: instCPU, DesiredCount: 1, OSProfile: "os", ConfigProfile: "cfg", CreatedAt: now.Add(2 * time.Second)},
		{ControlPlane: true, Name: "storage-cp", InstanceTypeID: instStorage, DesiredCount: 1, OSProfile: "os", ConfigProfile: "cfg", CreatedAt: now.Add(time.Second)},
		{ControlPlane: true, Name: "gpu-cp", InstanceTypeID: instGPU, DesiredCount: 1, OSProfile: "os", ConfigProfile: "cfg", CreatedAt: now},
	}

	cfg := db.MergeControlPlaneConfigFromNodePools(pools)
	require.NotNil(t, cfg)
	// Oldest control-plane pool has the highest priority.
	assert.Equal(t, []uuid.UUID{instGPU, instStorage, instCPU, instOther}, cfg.InstanceTypePreference)
}

func TestSyncClusterControlPlaneConfigFromCPNodePools_NoActiveCPPools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		clusterDeleting db.ConditionStatus
		wantErr         bool
	}{
		{
			name:            "cluster deleting allows empty active cp pools",
			clusterDeleting: db.ConditionStatusTrue,
			wantErr:         false,
		},
		{
			name:            "cluster not deleting returns error when no active cp pools",
			clusterDeleting: db.ConditionStatusFalse,
			wantErr:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.NodePool)(nil), (*db.ControlPlaneConfiguration)(nil))
			dbImpl := db.NewDB(bunDB)

			cluster := newValidCluster(withName("cluster-delete-case"))
			cluster.Conditions = db.ClusterConditions{
				Deleting: db.Condition{Status: tt.clusterDeleting},
			}
			_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
			require.NoError(t, err)

			cpPool := db.NodePool{
				ID:                         uuid.New(),
				ClusterID:                  cluster.ID,
				Name:                       "control-plane",
				NodeNamePrefix:             "cp",
				SubnetIDs:                  []uuid.UUID{},
				VpcPrefixIDs:               []uuid.UUID{},
				InstanceTypeID:             uuid.Nil,
				SSHKeyGroupIDs:             []uuid.UUID{},
				OSProfile:                  "",
				ConfigProfile:              "",
				SystemReservedMemory:       "1Gi",
				ControlPlane:               true,
				NodeLabels:                 []string{},
				KubernetesVersion:          "1.34.2",
				DiskSelectionStrategy:      "",
				CapiInfraMachineTemplate:   json.RawMessage(`{}`),
				CapiBootstrapFiles:         []db.CloudInitFile{},
				CapiPreKubeadmCommands:     []string{},
				CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
				Conditions: db.NodePoolConditions{
					Deleting: db.Condition{Status: db.ConditionStatusTrue},
				},
			}
			_, err = bunDB.NewInsert().Model(&cpPool).Exec(ctx)
			require.NoError(t, err)

			err = dbImpl.SyncClusterControlPlaneConfigFromCPNodePools(ctx, cluster.ID)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "expected at least one active control-plane node pool")
				return
			}
			require.NoError(t, err)
		})
	}
}

// Note: GetClusterForUpdate uses SQL SELECT ... FOR UPDATE which is
// Postgres-only; SQLite (used by SetupDB) rejects the syntax. Matches the
// existing GetNodePoolForUpdate pattern — no SQLite unit test. The behavior
// is exercised via MockTx in server-layer tests and end-to-end via the
// CAPD/Tilt local stack.

func TestSyncClusterControlPlaneConfigFromCPNodePools_PreservesClusterOverrides(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.NodePool)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)

	customInst := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000099")
	poolInst := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")

	cluster := newValidCluster(withName("cluster-preserve-cp-config"))
	cluster.ControlPlaneConfig = db.ControlPlaneConfig{
		MaxSurge: 1,
		ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
			InstanceTypePreference:      []uuid.UUID{customInst},
			ProvisioningMonitorDisabled: true,
		},
	}
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)
	initialSnapshot, err := dbImpl.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ClusterID: cluster.ID,
		Config: db.ControlPlaneConfigSpec{
			InstanceTypePreference:      []uuid.UUID{customInst},
			ProvisioningMonitorDisabled: true,
		},
	})
	require.NoError(t, err)
	_, err = bunDB.NewUpdate().
		Model(&db.Cluster{
			ID: cluster.ID,
			ControlPlaneConfig: db.ControlPlaneConfig{
				ConfigurationID: initialSnapshot.ID,
				DesiredCount:    0,
				MaxSurge:        1,
			},
		}).
		Column("configuration_id", "cp_desired_count", "cp_max_surge").
		WherePK().
		Exec(ctx)
	require.NoError(t, err)

	cpPool := db.NodePool{
		ID:                         uuid.New(),
		ClusterID:                  cluster.ID,
		Name:                       "control-plane",
		NodeNamePrefix:             "cp",
		SubnetIDs:                  []uuid.UUID{},
		VpcPrefixIDs:               []uuid.UUID{},
		InstanceTypeID:             poolInst,
		SSHKeyGroupIDs:             []uuid.UUID{},
		OSProfile:                  "os",
		ConfigProfile:              "cfg",
		SystemReservedMemory:       "1Gi",
		ControlPlane:               true,
		NodeLabels:                 []string{},
		KubernetesVersion:          "1.34.2",
		MaxSurge:                   0,
		DiskSelectionStrategy:      "",
		CapiInfraMachineTemplate:   json.RawMessage(`{}`),
		CapiBootstrapFiles:         []db.CloudInitFile{},
		CapiPreKubeadmCommands:     []string{},
		CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
		Conditions: db.NodePoolConditions{
			Deleting: db.Condition{Status: db.ConditionStatusFalse},
		},
	}
	_, err = bunDB.NewInsert().Model(&cpPool).Exec(ctx)
	require.NoError(t, err)

	err = dbImpl.SyncClusterControlPlaneConfigFromCPNodePools(ctx, cluster.ID)
	require.NoError(t, err)

	updated, err := dbImpl.GetCluster(ctx, cluster.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, updated.ControlPlaneConfig.MaxSurge)
	assert.Equal(t, []uuid.UUID{customInst, poolInst}, updated.ControlPlaneConfig.InstanceTypePreference)
	assert.True(t, updated.ControlPlaneConfig.ProvisioningMonitorDisabled)
}

func TestLoadClusterControlPlaneConfig_LoadsSnapshotConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)

	cluster := newValidCluster(withName("cluster-load-snapshot"))
	cluster.ControlPlaneConfig = db.ControlPlaneConfig{
		DesiredCount: 3,
		MaxSurge:     1,
	}
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	snapshotCfg := db.ControlPlaneConfigSpec{
		OSProfile:                   "ubuntu-24",
		DiskSelectionStrategy:       db.DiskSelectionStrategyLargest,
		ConfigProfile:               "cp-default",
		ProvisioningMonitorDisabled: true,
		InstanceTypePreference:      []uuid.UUID{uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")},
	}
	snapshot, err := dbImpl.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ClusterID: cluster.ID,
		Config:    snapshotCfg,
	})
	require.NoError(t, err)

	_, err = bunDB.NewUpdate().
		Model(&db.Cluster{
			ID: cluster.ID,
			ControlPlaneConfig: db.ControlPlaneConfig{
				ConfigurationID: snapshot.ID,
				DesiredCount:    3,
				MaxSurge:        1,
			},
		}).
		Column("configuration_id", "cp_desired_count", "cp_max_surge").
		WherePK().
		Exec(ctx)
	require.NoError(t, err)

	got, err := dbImpl.GetCluster(ctx, cluster.ID)
	require.NoError(t, err)
	assert.Equal(t, snapshot.ID, got.ControlPlaneConfig.ConfigurationID)
	assert.Equal(t, 3, got.ControlPlaneConfig.DesiredCount)
	assert.Equal(t, 1, got.ControlPlaneConfig.MaxSurge)
	assert.Equal(t, "ubuntu-24", got.ControlPlaneConfig.OSProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, got.ControlPlaneConfig.DiskSelectionStrategy)
	assert.Equal(t, "cp-default", got.ControlPlaneConfig.ConfigProfile)
	assert.True(t, got.ControlPlaneConfig.ProvisioningMonitorDisabled)
}

func TestCreateControlPlaneConfigRowIfChanged_NoOpWhenUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)

	cluster := newValidCluster(withName("cluster-publish-noop"))
	cluster.ControlPlaneConfig = db.ControlPlaneConfig{
		DesiredCount: 2,
		MaxSurge:     1,
	}
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	snapshotCfg := db.ControlPlaneConfigSpec{
		OSProfile:             "ubuntu-24",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
		ConfigProfile:         "cp-default",
	}
	require.NoError(t, db.CreateControlPlaneConfigRowIfChanged(ctx, dbImpl, &cluster, snapshotCfg, db.CreateControlPlaneConfigRowOpts{
		ChangeReason: "initial snapshot",
	}))
	require.NotEqual(t, uuid.Nil, cluster.ControlPlaneConfig.ConfigurationID)

	firstID := cluster.ControlPlaneConfig.ConfigurationID
	require.NoError(t, db.CreateControlPlaneConfigRowIfChanged(ctx, dbImpl, &cluster, snapshotCfg, db.CreateControlPlaneConfigRowOpts{
		ChangeReason: "repeat same snapshot",
	}))
	assert.Equal(t, firstID, cluster.ControlPlaneConfig.ConfigurationID)

	count, err := bunDB.NewSelect().TableExpr("control_plane_configurations").Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestCreateControlPlaneConfigRowIfChanged_CreatesRowWhenDiskStrategyChanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)

	cluster := newValidCluster(withName("cluster-publish-disk-strategy"))
	cluster.ControlPlaneConfig = db.ControlPlaneConfig{
		DesiredCount: 2,
		MaxSurge:     1,
	}
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	initial := db.ControlPlaneConfigSpec{
		OSProfile:             "ubuntu-24",
		ConfigProfile:         "cp-default",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	}
	require.NoError(t, db.CreateControlPlaneConfigRowIfChanged(ctx, dbImpl, &cluster, initial, db.CreateControlPlaneConfigRowOpts{
		ChangeReason: "initial snapshot",
	}))
	firstID := cluster.ControlPlaneConfig.ConfigurationID
	require.NotEqual(t, uuid.Nil, firstID)

	changed := initial
	changed.DiskSelectionStrategy = db.DiskSelectionStrategyLargest
	require.NoError(t, db.CreateControlPlaneConfigRowIfChanged(ctx, dbImpl, &cluster, changed, db.CreateControlPlaneConfigRowOpts{
		ChangeReason: "disk strategy changed",
	}))
	require.NotEqual(t, firstID, cluster.ControlPlaneConfig.ConfigurationID)
	assert.Equal(t, db.DiskSelectionStrategyLargest, cluster.ControlPlaneConfig.DiskSelectionStrategy)

	count, err := bunDB.NewSelect().TableExpr("control_plane_configurations").Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}
