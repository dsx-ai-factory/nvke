// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package migrations

import (
	"context"
	"database/sql"
	"fmt"
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
)

// TestBackfillNodeSetNVLinkPartition runs against the server named by
// NVKE_TEST_POSTGRES_DSN, in a database it creates and drops.
func TestBackfillNodeSetNVLinkPartition(t *testing.T) {
	ctx := context.Background()
	bunDB := newMigratedDatabase(ctx, t)

	cluster := db.Cluster{ID: uuid.New(), Name: "nvlink-backfill", KubernetesVersion: "1.34.2", ProviderType: db.ProviderTypeNico}
	cluster.SetNicoProvider(db.NicoClusterProvider{VpcID: uuid.New()})
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	insertPool := func(name string, nico *db.NicoNodePoolProvider) db.NodePool {
		pool := db.NodePool{ID: uuid.New(), ClusterID: cluster.ID, Name: name, NodeNamePrefix: name, Nico: nico}
		_, err := bunDB.NewInsert().Model(&pool).Exec(ctx)
		require.NoError(t, err)
		return pool
	}
	insertNodeSet := func(pool db.NodePool, nico *db.NicoNodePoolProvider) uuid.UUID {
		nodeSet := db.NodeSet{ID: uuid.New(), NodePoolID: pool.ID, KubernetesVersion: "1.34.2", Nico: nico}
		_, err := bunDB.NewInsert().Model(&nodeSet).Exec(ctx)
		require.NoError(t, err)
		return nodeSet.ID
	}

	poolPartition, otherPartition := uuid.New(), uuid.New()
	partitioned := insertPool("partitioned", &db.NicoNodePoolProvider{Release: "os", NvLinkLogicalPartitionID: &poolPartition})
	unpartitioned := insertPool("unpartitioned", &db.NicoNodePoolProvider{Release: "os"})

	missingKey := insertNodeSet(partitioned, &db.NicoNodePoolProvider{Release: "os"})
	ownPartition := insertNodeSet(partitioned, &db.NicoNodePoolProvider{Release: "os", NvLinkLogicalPartitionID: &otherPartition})
	noPayload := insertNodeSet(partitioned, nil)
	poolWithout := insertNodeSet(unpartitioned, &db.NicoNodePoolProvider{Release: "os"})

	// The second run checks that the migration is idempotent.
	for run := range 2 {
		require.NoError(t, backfillNodeSetNVLinkPartition(ctx, bunDB), "run %d", run+1)
	}

	partitionOf := func(id uuid.UUID) *uuid.UUID {
		var nodeSet db.NodeSet
		require.NoError(t, bunDB.NewSelect().Model(&nodeSet).Where("id = ?", id).Scan(ctx))
		if nodeSet.Nico == nil {
			return nil
		}
		return nodeSet.Nico.NvLinkLogicalPartitionID
	}
	assert.Equal(t, &poolPartition, partitionOf(missingKey), "node set without the key takes the pool's partition")
	assert.Equal(t, &otherPartition, partitionOf(ownPartition), "node set with a partition keeps it")
	assert.Nil(t, partitionOf(noPayload), "node set without a nico payload is left alone")
	assert.Nil(t, partitionOf(poolWithout), "node set of a pool without a partition gets none")

	var stillNull bool
	require.NoError(t, bunDB.NewRaw("SELECT nico IS NULL FROM node_sets WHERE id = ?", noPayload).Scan(ctx, &stillNull))
	assert.True(t, stillNull, "a NULL payload stays NULL")
}

func newMigratedDatabase(ctx context.Context, t *testing.T) *bun.DB {
	t.Helper()

	adminDSN := os.Getenv("NVKE_TEST_POSTGRES_DSN")
	if adminDSN == "" {
		t.Skip("NVKE_TEST_POSTGRES_DSN not set")
	}

	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN)))
	defer admin.Close()

	name := "nvlinkbf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	require.NoError(t, Migrate(ctx, bunDB, ""))
	return bunDB
}
