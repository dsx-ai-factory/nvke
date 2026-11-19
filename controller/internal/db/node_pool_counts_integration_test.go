// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package db_test

import (
	"context"
	"database/sql"
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
	exerciseNodePoolCounts(t, database)
}
