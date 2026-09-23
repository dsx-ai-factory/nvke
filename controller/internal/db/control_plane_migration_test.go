// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// TestMigrateControlPlaneToControllerFlagOffIsNoop verifies the flag-off path
// short-circuits before touching the DB: a nil *bun.DB would panic if it were
// dereferenced, so a clean return proves the guard runs first. This is the only
// branch exercisable without a live Postgres; the SQL body is validated in
// staging against a real database.
func TestMigrateControlPlaneToControllerFlagOffIsNoop(t *testing.T) {
	require.NotPanics(t, func() {
		err := db.MigrateControlPlaneToController(context.Background(), nil, false)
		require.NoError(t, err)
	})
}

func TestSyncMissingControlPlaneConfigsFromCPNodePools(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.NodePool)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)

	imported := newValidCluster(withName("imported-site"))
	_, err := bunDB.NewInsert().Model(&imported).Exec(ctx)
	require.NoError(t, err)
	importedPool := importedControlPlanePool(imported.ID, 1)
	_, err = bunDB.NewInsert().Model(&importedPool).Exec(ctx)
	require.NoError(t, err)

	alreadySynced := newValidCluster(withName("already-synced"))
	_, err = bunDB.NewInsert().Model(&alreadySynced).Exec(ctx)
	require.NoError(t, err)
	existingSnapshot, err := dbImpl.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ClusterID: alreadySynced.ID,
		Config: db.ControlPlaneConfigSpec{
			Nico: &db.NicoControlPlaneProvider{OSProfile: "existing-os"},
		},
		ChangeReason: "already present",
	})
	require.NoError(t, err)
	_, err = bunDB.NewUpdate().
		Model(&db.Cluster{
			ID: alreadySynced.ID,
			ControlPlaneConfig: db.ControlPlaneConfig{
				ConfigurationID: existingSnapshot.ID,
				DesiredCount:    3,
			},
		}).
		Column("configuration_id", "cp_desired_count").
		WherePK().
		Exec(ctx)
	require.NoError(t, err)
	syncedPool := importedControlPlanePool(alreadySynced.ID, 3)
	_, err = bunDB.NewInsert().Model(&syncedPool).Exec(ctx)
	require.NoError(t, err)

	deleting := newValidCluster(withName("deleting-site"))
	deleting.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	_, err = bunDB.NewInsert().Model(&deleting).Exec(ctx)
	require.NoError(t, err)
	deletingPool := importedControlPlanePool(deleting.ID, 1)
	_, err = bunDB.NewInsert().Model(&deletingPool).Exec(ctx)
	require.NoError(t, err)

	require.NoError(t, db.SyncMissingControlPlaneConfigsFromCPNodePools(ctx, bunDB))

	healed, err := dbImpl.GetCluster(ctx, imported.ID)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, healed.ControlPlaneConfig.ConfigurationID)
	assert.Equal(t, 1, healed.ControlPlaneConfig.DesiredCount)
	require.NotNil(t, healed.ControlPlaneConfig.Nico)
	assert.Equal(t, "nke-sc-os", healed.ControlPlaneConfig.Nico.OSProfile)
	assert.Equal(t, "nke-sc-cp", healed.ControlPlaneConfig.Nico.ConfigProfile)

	unchanged, err := dbImpl.GetCluster(ctx, alreadySynced.ID)
	require.NoError(t, err)
	assert.Equal(t, existingSnapshot.ID, unchanged.ControlPlaneConfig.ConfigurationID)
	assert.Equal(t, 3, unchanged.ControlPlaneConfig.DesiredCount)

	stillMissing, err := dbImpl.GetCluster(ctx, deleting.ID)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, stillMissing.ControlPlaneConfig.ConfigurationID)
}

func importedControlPlanePool(clusterID uuid.UUID, desired int) db.NodePool {
	pool := db.NodePool{
		ID:                   uuid.New(),
		ClusterID:            clusterID,
		Name:                 "nke-tier-0-controlplane",
		NodeNamePrefix:       "cp",
		SystemReservedMemory: "16Gi",
		ControlPlane:         true,
		NodeLabels:           []string{},
		KubernetesVersion:    "1.34.2",
		DesiredCount:         desired,
		Conditions: db.NodePoolConditions{
			Deleting: db.Condition{Status: db.ConditionStatusFalse},
		},
	}
	pool.SetNico(db.NicoNodePoolProvider{
		OSProfile:             "nke-sc-os",
		ConfigProfile:         "nke-sc-cp",
		InstanceTypeID:        uuid.MustParse("d1a6401c-6b19-4261-92c3-79555cb56462"),
		SubnetIDs:             []uuid.UUID{uuid.MustParse("53c42b79-3dbf-40e8-9363-75228d7b6e1b")},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	return pool
}
