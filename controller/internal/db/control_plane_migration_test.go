// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

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
			Nico: &db.NicoControlPlaneProvider{Release: "existing-os"},
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
	assert.Equal(t, "nvke-sc-os", healed.ControlPlaneConfig.Nico.Release)
	assert.Equal(t, "nvke-sc-cp", healed.ControlPlaneConfig.Nico.ConfigProfile)

	unchanged, err := dbImpl.GetCluster(ctx, alreadySynced.ID)
	require.NoError(t, err)
	assert.Equal(t, existingSnapshot.ID, unchanged.ControlPlaneConfig.ConfigurationID)
	assert.Equal(t, 3, unchanged.ControlPlaneConfig.DesiredCount)

	stillMissing, err := dbImpl.GetCluster(ctx, deleting.ID)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, stillMissing.ControlPlaneConfig.ConfigurationID)
}

// TestMigrateControlPlaneToControllerPostgres preserves each generation's OVS
// intent while migrating ownership, using only session-local PostgreSQL tables.
func TestMigrateControlPlaneToControllerPostgres(t *testing.T) {
	dsn := os.Getenv("NKE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NKE_TEST_POSTGRES_DSN to run the PostgreSQL migration test")
	}
	const (
		fabric = `{"uplink_interface":"eth1","bridge_name":"br-fabric"}`
		uplink = `{"uplink_interface":"eth2","bridge_name":"br-fabric"}`
		bridge = `{"uplink_interface":"eth1","bridge_name":"br-workload"}`
	)
	tests := []struct {
		name        string
		desired     string                        // Empty means the snapshot has no open_v_switch key.
		generations []postgresMigrationGeneration // The first generation is current.
	}{
		{
			name:        "equal intent with different key order",
			desired:     `{"bridge_name":"br-fabric","uplink_interface":"eth1"}`,
			generations: []postgresMigrationGeneration{{openVSwitch: fabric, nodeCount: 1}},
		},
		{
			name:        "current generation has a different uplink",
			desired:     uplink,
			generations: []postgresMigrationGeneration{{openVSwitch: fabric, nodeCount: 2, wantOwnSnapshot: true}},
		},
		{
			name:        "current generation has a different bridge",
			desired:     bridge,
			generations: []postgresMigrationGeneration{{openVSwitch: fabric, nodeCount: 1, wantOwnSnapshot: true}},
		},
		{
			name:        "explicit generation and automatic desired selection",
			generations: []postgresMigrationGeneration{{openVSwitch: fabric, nodeCount: 1, wantOwnSnapshot: true}},
		},
		{
			name:        "explicit generation and JSON null desired selection",
			desired:     "null",
			generations: []postgresMigrationGeneration{{openVSwitch: fabric, nodeCount: 1, wantOwnSnapshot: true}},
		},
		{
			name:        "automatic generation and explicit desired selection",
			desired:     fabric,
			generations: []postgresMigrationGeneration{{nodeCount: 1, wantOwnSnapshot: true}},
		},
		{
			name:        "JSON null generation and explicit desired selection",
			desired:     fabric,
			generations: []postgresMigrationGeneration{{openVSwitch: "null", nodeCount: 1, wantOwnSnapshot: true}},
		},
		{
			name:    "deprecated generation with equal intent gets its own snapshot",
			desired: fabric,
			generations: []postgresMigrationGeneration{
				{openVSwitch: fabric, nodeCount: 1},
				{openVSwitch: fabric, nodeCount: 2, wantOwnSnapshot: true},
			},
		},
		{
			name:    "deprecated generations retain separate shared snapshots",
			desired: bridge,
			generations: []postgresMigrationGeneration{
				{openVSwitch: bridge, nodeCount: 1},
				{openVSwitch: fabric, nodeCount: 2, wantOwnSnapshot: true},
				{openVSwitch: uplink, nodeCount: 2, wantOwnSnapshot: true},
			},
		},
		{
			name:        "SQL NULL generation equals missing key desired selection",
			generations: []postgresMigrationGeneration{{nodeCount: 1}},
		},
		{
			name:        "SQL NULL generation equals JSON null desired selection",
			desired:     "null",
			generations: []postgresMigrationGeneration{{nodeCount: 1}},
		},
		{
			name:        "JSON null generation equals missing key desired selection",
			generations: []postgresMigrationGeneration{{openVSwitch: "null", nodeCount: 1}},
		},
		{
			name:        "JSON null generation equals JSON null desired selection",
			desired:     "null",
			generations: []postgresMigrationGeneration{{openVSwitch: "null", nodeCount: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			database := postgresMigrationDB(ctx, t, dsn)
			desiredID, desiredJSON, generations := seedPostgresMigration(ctx, t, database, tt.desired, tt.generations)
			require.NoError(t, db.MigrateControlPlaneToController(ctx, database, slog.Default()))
			nodes := postgresMigrationNodes(ctx, t, database)
			byID := make(map[uuid.UUID]postgresMigrationNode, len(nodes))
			for _, node := range nodes {
				byID[node.ID] = node
				assert.Nil(t, node.NodePoolID)
				assert.Nil(t, node.NodeSetID)
			}
			expectedNodes, expectedSnapshots := 0, 1
			seenSnapshots := map[uuid.UUID]bool{desiredID: true}
			for _, generation := range generations {
				expectedNodes += generation.nodeCount
				var generationSnapshot uuid.UUID
				for _, nodeID := range generation.nodeIDs {
					node, found := byID[nodeID]
					require.True(t, found, "every node must resolve its pinned snapshot")
					if generationSnapshot == uuid.Nil {
						generationSnapshot = node.ConfigurationID
					}
					assert.Equal(t, generationSnapshot, node.ConfigurationID, "nodes in one generation share a snapshot")
					if !generation.wantOwnSnapshot {
						assert.Equal(t, desiredID, node.ConfigurationID)
					}
					var actual db.ControlPlaneConfigSpec
					require.NoError(t, json.Unmarshal(node.Config, &actual))
					var expected *db.OpenVSwitchConfig
					if generation.openVSwitch != "" {
						require.NoError(t, json.Unmarshal([]byte(generation.openVSwitch), &expected))
					}
					assert.Equal(t, expected, actual.OpenVSwitch)
					assert.Equal(t, &db.NicoControlPlaneProvider{Release: "os", ConfigProfile: "cp"}, actual.Nico)
					assert.Equal(t, "1.34.2", actual.KubernetesVersion)
					assert.Equal(t, "16Gi", actual.SystemReservedMemory)
					assert.Equal(t, map[string]string{"preserved": ""}, actual.NodeLabels)
				}
				if generation.wantOwnSnapshot {
					expectedSnapshots++
					assert.NotContains(t, seenSnapshots, generationSnapshot, "each deprecated or differing generation gets its own snapshot")
					seenSnapshots[generationSnapshot] = true
				}
			}
			assert.Len(t, nodes, expectedNodes)
			assertPostgresMigrationRows(ctx, t, database, desiredID, desiredJSON, expectedSnapshots)
			require.NoError(t, db.MigrateControlPlaneToController(ctx, database, slog.Default()))
			assert.Equal(t, nodes, postgresMigrationNodes(ctx, t, database), "rerunning must preserve all pinned IDs and settings")
			assertPostgresMigrationRows(ctx, t, database, desiredID, desiredJSON, expectedSnapshots)
		})
	}
}

func importedControlPlanePool(clusterID uuid.UUID, desired int) db.NodePool {
	pool := db.NodePool{
		ID:                   uuid.New(),
		ClusterID:            clusterID,
		Name:                 "nvke-tier-0-controlplane",
		NodeNamePrefix:       "cp",
		SystemReservedMemory: "16Gi",
		ControlPlane:         true,
		NodeLabels:           map[string]string{},
		KubernetesVersion:    "1.34.2",
		DesiredCount:         desired,
		Conditions: db.NodePoolConditions{
			Deleting: db.Condition{Status: db.ConditionStatusFalse},
		},
	}
	pool.SetNico(db.NicoNodePoolProvider{
		Release:               "nvke-sc-os",
		ConfigProfile:         "nvke-sc-cp",
		InstanceTypeID:        uuid.MustParse("00000000-0000-4000-8000-000000000008"),
		SubnetIDs:             []uuid.UUID{uuid.MustParse("00000000-0000-4000-8000-000000000003")},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	return pool
}

type postgresMigrationGeneration struct {
	openVSwitch     string // Empty means SQL NULL; "null" means JSON null.
	nodeCount       int
	wantOwnSnapshot bool
	nodeIDs         []uuid.UUID
}

type postgresMigrationNode struct {
	ID              uuid.UUID       `bun:"id"`
	NodePoolID      *uuid.UUID      `bun:"node_pool_id"`
	NodeSetID       *uuid.UUID      `bun:"node_set_id"`
	ConfigurationID uuid.UUID       `bun:"configuration_id"`
	Config          json.RawMessage `bun:"config"`
}

func postgresMigrationDB(ctx context.Context, t *testing.T, dsn string) *bun.DB {
	t.Helper()
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": "pg_temp"})))
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	database := bun.NewDB(sqldb, pgdialect.New())
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("could not connect using NKE_TEST_POSTGRES_DSN")
	}
	// Every table is temporary and lives on the single pooled connection. Real
	// foreign keys also exercise the migration's insert, detach, and delete order.
	for _, statement := range []string{
		`CREATE TEMP TABLE clusters (
			id uuid PRIMARY KEY, configuration_id uuid, conditions jsonb NOT NULL DEFAULT '{}',
			name text NOT NULL DEFAULT 'ovs-migration-test'
		)`,
		`CREATE TEMP TABLE control_plane_configurations (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), cluster_id uuid NOT NULL REFERENCES clusters(id),
			config jsonb NOT NULL, change_reason text, created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TEMP TABLE node_pools (
			id uuid PRIMARY KEY, cluster_id uuid NOT NULL REFERENCES clusters(id),
			control_plane boolean NOT NULL, current_node_set_id uuid, conditions jsonb NOT NULL DEFAULT '{}'
		)`,
		`CREATE TEMP TABLE node_sets (
			id uuid PRIMARY KEY, node_pool_id uuid NOT NULL REFERENCES node_pools(id),
			nico jsonb, kubernetes_version text NOT NULL,
			open_v_switch jsonb
		)`,
		`CREATE TEMP TABLE nodes (
			id uuid PRIMARY KEY, cluster_id uuid NOT NULL REFERENCES clusters(id),
			node_pool_id uuid REFERENCES node_pools(id), node_set_id uuid REFERENCES node_sets(id),
			control_plane boolean NOT NULL, configuration_id uuid REFERENCES control_plane_configurations(id),
			updated_at timestamptz NOT NULL DEFAULT now()
		)`,
		`ALTER TABLE clusters ADD FOREIGN KEY (configuration_id) REFERENCES control_plane_configurations(id)`,
		`ALTER TABLE node_pools ADD FOREIGN KEY (current_node_set_id) REFERENCES node_sets(id)`,
	} {
		_, err := database.ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	return database
}

func seedPostgresMigration(
	ctx context.Context,
	t *testing.T,
	database *bun.DB,
	desired string,
	generations []postgresMigrationGeneration,
) (uuid.UUID, []byte, []postgresMigrationGeneration) {
	t.Helper()
	clusterID, poolID, desiredID := uuid.New(), uuid.New(), uuid.New()
	config := map[string]any{
		"kubernetes_version":     "1.34.2",
		"system_reserved_memory": "16Gi",
		"node_labels":            map[string]string{"preserved": ""},
		"nico": map[string]string{
			"os_profile":     "os",
			"config_profile": "cp",
		},
	}
	if desired != "" {
		config["open_v_switch"] = json.RawMessage(desired)
	}
	desiredJSON, err := json.Marshal(config)
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, "INSERT INTO clusters (id) VALUES (?)", clusterID)
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, "INSERT INTO control_plane_configurations (id, cluster_id, config) VALUES (?, ?, ?::jsonb)", desiredID, clusterID, string(desiredJSON))
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, "UPDATE clusters SET configuration_id = ? WHERE id = ?", desiredID, clusterID)
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, "INSERT INTO node_pools (id, cluster_id, control_plane) VALUES (?, ?, true)", poolID, clusterID)
	require.NoError(t, err)
	seeded := append([]postgresMigrationGeneration(nil), generations...)
	for i := range seeded {
		generation := &seeded[i]
		setID := uuid.New()
		var network any
		if generation.openVSwitch != "" {
			network = generation.openVSwitch
		}
		_, err = database.ExecContext(ctx, `INSERT INTO node_sets (id, node_pool_id, nico, kubernetes_version, open_v_switch)
			VALUES (?, ?, '{"os_profile":"os","config_profile":"cp"}'::jsonb, '1.34.2', ?::jsonb)`, setID, poolID, network)
		require.NoError(t, err)
		if i == 0 {
			_, err = database.ExecContext(ctx, "UPDATE node_pools SET current_node_set_id = ? WHERE id = ?", setID, poolID)
			require.NoError(t, err)
		}
		for range generation.nodeCount {
			nodeID := uuid.New()
			generation.nodeIDs = append(generation.nodeIDs, nodeID)
			_, err = database.ExecContext(ctx, `INSERT INTO nodes (id, cluster_id, node_pool_id, node_set_id, control_plane)
				VALUES (?, ?, ?, ?, true)`, nodeID, clusterID, poolID, setID)
			require.NoError(t, err)
		}
	}
	return desiredID, desiredJSON, seeded
}

func postgresMigrationNodes(ctx context.Context, t *testing.T, database *bun.DB) []postgresMigrationNode {
	t.Helper()
	var nodes []postgresMigrationNode
	err := database.NewRaw(`SELECT n.id, n.node_pool_id, n.node_set_id, n.configuration_id, cfg.config
		FROM nodes n JOIN control_plane_configurations cfg ON cfg.id = n.configuration_id ORDER BY n.id`).Scan(ctx, &nodes)
	require.NoError(t, err)
	return nodes
}

func assertPostgresMigrationRows(ctx context.Context, t *testing.T, database *bun.DB, desiredID uuid.UUID, desiredJSON []byte, snapshots int) {
	t.Helper()
	var currentID uuid.UUID
	require.NoError(t, database.NewRaw("SELECT configuration_id FROM clusters").Scan(ctx, &currentID))
	assert.Equal(t, desiredID, currentID, "migration must preserve the cluster's desired snapshot pointer")
	var actualJSON string
	require.NoError(t, database.NewRaw("SELECT config FROM control_plane_configurations WHERE id = ?", desiredID).Scan(ctx, &actualJSON))
	assert.JSONEq(t, string(desiredJSON), actualJSON, "migration must not change the desired snapshot")
	for _, table := range []string{"node_pools", "node_sets", "control_plane_configurations"} {
		var count int
		require.NoError(t, database.NewSelect().Table(table).ColumnExpr("count(*)").Scan(ctx, &count))
		want := 0
		if table == "control_plane_configurations" {
			want = snapshots
		}
		assert.Equal(t, want, count, table)
	}
}
