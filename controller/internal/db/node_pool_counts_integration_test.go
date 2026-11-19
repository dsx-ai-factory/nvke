// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TestListNodePoolCountsPostgres exercises the production SQL dialect against
// isolated tables. Migration coverage remains in the migration integration suite.
func TestListNodePoolCountsPostgres(t *testing.T) {
	dsn := os.Getenv(postgresDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set", postgresDSNEnv)
	}
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	name := "poolcounts_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
	})
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	parsed.Path = "/" + name
	database := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(parsed.String()))), pgdialect.New())
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	for _, model := range []any{(*db.NodeSet)(nil), (*db.Node)(nil)} {
		_, err := database.NewCreateTable().Model(model).Exec(t.Context())
		require.NoError(t, err)
	}
	for _, tt := range []struct {
		name, desired, stored string
		updated               int
	}{
		{"equivalent exponent", `{"value":1e-7}`, `{"value":1e-7}`, 1},
		{"distinct large integers", `{"value":9007199254740992}`, `{"value":9007199254740993}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := db.NodePool{ID: uuid.New(), SystemReservedMemory: "16Gi"}
			pool.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(tt.desired)})
			nodeSet := db.NodeSet{ID: uuid.New(), NodePoolID: pool.ID, SystemReservedMemory: pool.SystemReservedMemory}
			nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(tt.stored)})
			_, err := database.NewInsert().Model(&nodeSet).Exec(t.Context())
			require.NoError(t, err)
			// Verify the comparison actually crosses a JSONB round trip.
			require.NoError(t, database.NewSelect().Model(&nodeSet).WherePK().Scan(t.Context()))
			if tt.updated == 1 {
				require.Contains(t, string(nodeSet.Capi.InfraMachineTemplate), "0.0000001")
			}
			node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), NodePoolID: pool.ID, NodeSetID: nodeSet.ID}
			_, err = database.NewInsert().Model(&node).Exec(t.Context())
			require.NoError(t, err)
			counts, err := db.NewDB(database).ListNodePoolCounts(t.Context(), []db.NodePool{pool})
			require.NoError(t, err)
			require.Equal(t, db.NodePoolCounts{NodeCount: 1, UpdatedNodeCount: tt.updated}, counts[pool.ID])
		})
	}

	exerciseNodePoolCounts(t, database)
}
