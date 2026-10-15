// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/db/migrations"
)

const postgresDSNEnv = "NVKE_TEST_POSTGRES_DSN"

// newMigratedPostgres creates an empty database on the server named by
// NVKE_TEST_POSTGRES_DSN and applies the schema migrations to it.
func newMigratedPostgres(ctx context.Context, t *testing.T) *bun.DB {
	t.Helper()

	adminDSN := os.Getenv(postgresDSNEnv)
	if adminDSN == "" {
		t.Skipf("%s not set", postgresDSNEnv)
	}

	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN)))
	defer admin.Close()

	name := "cpmig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := admin.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)

	u, err := url.Parse(adminDSN)
	require.NoError(t, err)
	u.Path = "/" + name

	bunDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(u.String()))), pgdialect.New())
	t.Cleanup(func() {
		bunDB.Close()
		cleanup := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN)))
		defer cleanup.Close()
		_, _ = cleanup.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})

	require.NoError(t, migrations.Migrate(ctx, bunDB))
	return bunDB
}

type cpMigrationFixture struct {
	t     *testing.T
	ctx   context.Context
	bunDB *bun.DB
	dbi   db.DB
}

func (f *cpMigrationFixture) insertCluster(name string, deleting bool) db.Cluster {
	f.t.Helper()
	cluster := newValidCluster(withName(name))
	if deleting {
		cluster.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	}
	_, err := f.bunDB.NewInsert().Model(&cluster).Exec(f.ctx)
	require.NoError(f.t, err)
	return cluster
}

func (f *cpMigrationFixture) setSnapshot(cluster *db.Cluster, osProfile, k8sVersion string) uuid.UUID {
	f.t.Helper()
	snapshot := db.ControlPlaneConfiguration{
		ClusterID: cluster.ID,
		Config: db.ControlPlaneConfigSpec{
			KubernetesVersion: k8sVersion,
			Nico:              &db.NicoControlPlaneProvider{OSProfile: osProfile, ConfigProfile: "cp-config"},
		},
		ChangeReason: "test",
	}
	created, err := f.dbi.CreateControlPlaneConfiguration(f.ctx, &snapshot)
	require.NoError(f.t, err)

	cluster.ControlPlaneConfig.ConfigurationID = created.ID
	cluster.ControlPlaneConfig.DesiredCount = 1
	_, err = f.bunDB.NewUpdate().
		Model(cluster).
		Column("configuration_id", "cp_desired_count").
		WherePK().
		Exec(f.ctx)
	require.NoError(f.t, err)
	return created.ID
}

// insertPool inserts a control-plane (or worker) pool with a current node set.
func (f *cpMigrationFixture) insertPool(clusterID uuid.UUID, controlPlane, deleting bool) (db.NodePool, db.NodeSet) {
	f.t.Helper()
	pool := importedControlPlanePool(clusterID, 1)
	pool.Name = "pool-" + uuid.NewString()[:8]
	pool.ControlPlane = controlPlane
	if deleting {
		pool.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	}
	_, err := f.bunDB.NewInsert().Model(&pool).Exec(f.ctx)
	require.NoError(f.t, err)

	set := f.insertNodeSet(pool, "nvke-sc-os", "1.34.2")
	pool.CurrentNodeSetID = &set.ID
	_, err = f.bunDB.NewUpdate().Model(&pool).Column("current_node_set_id").WherePK().Exec(f.ctx)
	require.NoError(f.t, err)
	return pool, set
}

func (f *cpMigrationFixture) insertNodeSet(pool db.NodePool, osProfile, k8sVersion string) db.NodeSet {
	f.t.Helper()
	provider := *pool.Nico
	provider.OSProfile = osProfile
	set := db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           pool.ID,
		DesiredCount:         1,
		KubernetesVersion:    k8sVersion,
		SystemReservedMemory: "16Gi",
		Nico:                 &provider,
	}
	_, err := f.bunDB.NewInsert().Model(&set).Exec(f.ctx)
	require.NoError(f.t, err)
	return set
}

func (f *cpMigrationFixture) insertNode(clusterID uuid.UUID, controlPlane bool, pool *db.NodePool, set *db.NodeSet, configID uuid.UUID) db.Node {
	f.t.Helper()
	node := db.Node{
		ID:                          uuid.New(),
		ClusterID:                   clusterID,
		ControlPlane:                controlPlane,
		Name:                        "node-" + uuid.NewString()[:8],
		IP:                          "10.0.0.1",
		SpireAttestationMethod:      db.SpireAttestationMethodTPM,
		ControlPlaneConfigurationID: configID,
	}
	if pool != nil {
		node.NodePoolID = pool.ID
	}
	if set != nil {
		node.NodeSetID = set.ID
	}
	_, err := f.bunDB.NewInsert().Model(&node).Exec(f.ctx)
	require.NoError(f.t, err)
	return node
}

func (f *cpMigrationFixture) node(id uuid.UUID) db.Node {
	f.t.Helper()
	n, err := f.dbi.GetNode(f.ctx, id)
	require.NoError(f.t, err)
	return n
}

func (f *cpMigrationFixture) count(table string, id uuid.UUID) int {
	f.t.Helper()
	n, err := f.bunDB.NewSelect().Table(table).Where("id = ?", id).Count(f.ctx)
	require.NoError(f.t, err)
	return int(n)
}

func (f *cpMigrationFixture) snapshot(id uuid.UUID) db.ControlPlaneConfiguration {
	f.t.Helper()
	var cfg db.ControlPlaneConfiguration
	require.NoError(f.t, f.bunDB.NewSelect().Model(&cfg).Where("id = ?", id).Scan(f.ctx))
	return cfg
}

func TestMigrateControlPlaneToController(t *testing.T) {
	ctx := context.Background()
	bunDB := newMigratedPostgres(ctx, t)
	f := &cpMigrationFixture{t: t, ctx: ctx, bunDB: bunDB, dbi: db.NewDB(bunDB)}

	// Imported site: pooled CP node, no snapshot yet.
	imported := f.insertCluster("imported", false)
	importedPool, importedSet := f.insertPool(imported.ID, true, false)
	importedNode := f.insertNode(imported.ID, true, &importedPool, &importedSet, uuid.Nil)

	// In-flight upgrade: a current-generation node and a node left on a
	// deprecated node set, plus a worker pool that must be left alone.
	upgrading := f.insertCluster("upgrading", false)
	upgradingSnapshotID := f.setSnapshot(&upgrading, "new-os", "1.35.0")
	upgradingPool, currentSet := f.insertPool(upgrading.ID, true, false)
	deprecatedSet := f.insertNodeSet(upgradingPool, "old-os", "1.34.2")
	currentNode := f.insertNode(upgrading.ID, true, &upgradingPool, &currentSet, uuid.Nil)
	deprecatedNode := f.insertNode(upgrading.ID, true, &upgradingPool, &deprecatedSet, uuid.Nil)
	workerPool, workerSet := f.insertPool(upgrading.ID, false, false)
	workerNode := f.insertNode(upgrading.ID, false, &workerPool, &workerSet, uuid.Nil)

	// Already enrolled: no pooled nodes.
	enrolled := f.insertCluster("enrolled", false)
	enrolledSnapshotID := f.setSnapshot(&enrolled, "enrolled-os", "1.35.0")
	enrolledNode := f.insertNode(enrolled.ID, true, nil, nil, enrolledSnapshotID)

	// Deleting cluster: left as is.
	deleting := f.insertCluster("deleting", true)
	deletingPool, deletingSet := f.insertPool(deleting.ID, true, false)
	deletingNode := f.insertNode(deleting.ID, true, &deletingPool, &deletingSet, uuid.Nil)

	// No snapshot and only a deleting CP pool: nothing to enroll from, must not
	// block startup.
	orphaned := f.insertCluster("orphaned", false)
	orphanedPool, orphanedSet := f.insertPool(orphaned.ID, true, true)
	orphanedNode := f.insertNode(orphaned.ID, true, &orphanedPool, &orphanedSet, uuid.Nil)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	require.NoError(t, db.MigrateControlPlaneToController(ctx, bunDB, logger))

	t.Run("imported cluster gets a snapshot and is re-parented", func(t *testing.T) {
		cluster, err := f.dbi.GetCluster(ctx, imported.ID)
		require.NoError(t, err)
		require.NotEqual(t, uuid.Nil, cluster.ControlPlaneConfig.ConfigurationID)

		node := f.node(importedNode.ID)
		assert.True(t, node.IsManagedByControlPlaneController())
		assert.Equal(t, uuid.Nil, node.NodeSetID)
		assert.Equal(t, cluster.ControlPlaneConfig.ConfigurationID, node.ControlPlaneConfigurationID)
		assert.Equal(t, "nvke-sc-os", f.snapshot(node.ControlPlaneConfigurationID).Config.Nico.OSProfile)
	})

	t.Run("current generation pins to the cluster snapshot", func(t *testing.T) {
		node := f.node(currentNode.ID)
		assert.True(t, node.IsManagedByControlPlaneController())
		assert.Equal(t, upgradingSnapshotID, node.ControlPlaneConfigurationID)
	})

	t.Run("deprecated generation gets its own snapshot so the rollout finishes", func(t *testing.T) {
		node := f.node(deprecatedNode.ID)
		assert.True(t, node.IsManagedByControlPlaneController())
		assert.NotEqual(t, upgradingSnapshotID, node.ControlPlaneConfigurationID)

		snap := f.snapshot(node.ControlPlaneConfigurationID)
		assert.Equal(t, upgrading.ID, snap.ClusterID)
		assert.Equal(t, "1.34.2", snap.Config.KubernetesVersion)
		assert.Equal(t, "old-os", snap.Config.Nico.OSProfile)
		assert.Equal(t, "nvke-sc-cp", snap.Config.Nico.ConfigProfile)
	})

	t.Run("emptied control-plane pools and node sets are deleted", func(t *testing.T) {
		for _, id := range []uuid.UUID{importedPool.ID, upgradingPool.ID} {
			assert.Zero(t, f.count("node_pools", id), "pool %s", id)
		}
		for _, id := range []uuid.UUID{importedSet.ID, currentSet.ID, deprecatedSet.ID} {
			assert.Zero(t, f.count("node_sets", id), "node set %s", id)
		}
	})

	t.Run("worker pools and nodes are untouched", func(t *testing.T) {
		assert.Equal(t, 1, f.count("node_pools", workerPool.ID))
		assert.Equal(t, 1, f.count("node_sets", workerSet.ID))
		node := f.node(workerNode.ID)
		assert.Equal(t, workerPool.ID, node.NodePoolID)
		assert.Equal(t, workerSet.ID, node.NodeSetID)
	})

	t.Run("already enrolled cluster is unchanged", func(t *testing.T) {
		node := f.node(enrolledNode.ID)
		assert.True(t, node.IsManagedByControlPlaneController())
		assert.Equal(t, enrolledSnapshotID, node.ControlPlaneConfigurationID)
	})

	t.Run("deleting cluster keeps its pooled nodes and pools", func(t *testing.T) {
		node := f.node(deletingNode.ID)
		assert.Equal(t, deletingPool.ID, node.NodePoolID)
		assert.Equal(t, deletingSet.ID, node.NodeSetID)
		assert.Equal(t, 1, f.count("node_pools", deletingPool.ID))
	})

	t.Run("cluster with no snapshot source is skipped without blocking startup", func(t *testing.T) {
		node := f.node(orphanedNode.ID)
		assert.Equal(t, orphanedPool.ID, node.NodePoolID)
		assert.Equal(t, 1, f.count("node_pools", orphanedPool.ID))
	})

	t.Run("skipped cluster is logged and others are not", func(t *testing.T) {
		out := logs.String()
		assert.Contains(t, out, "skipped cluster with no snapshot source")
		assert.Contains(t, out, orphaned.ID.String())
		assert.NotContains(t, out, imported.ID.String())
		assert.NotContains(t, out, deleting.ID.String())
	})

	t.Run("second run changes nothing", func(t *testing.T) {
		before := f.node(deprecatedNode.ID)
		require.NoError(t, db.MigrateControlPlaneToController(ctx, bunDB, logger))
		after := f.node(deprecatedNode.ID)
		assert.Equal(t, before.ControlPlaneConfigurationID, after.ControlPlaneConfigurationID)
		assert.Equal(t, 1, f.count("node_pools", workerPool.ID))
	})
}
