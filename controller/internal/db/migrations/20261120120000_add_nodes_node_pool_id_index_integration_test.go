// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/migrate"
)

func TestNodesNodePoolIDIndex(t *testing.T) {
	ctx := t.Context()
	database := newMigratedDatabase(ctx, t)
	assertIndex := func() {
		t.Helper()
		var definition string
		require.NoError(t, database.NewRaw(`
			SELECT indexdef FROM pg_indexes
			WHERE schemaname = 'public' AND tablename = 'nodes' AND indexname = 'nodes_node_pool_id_idx'
		`).Scan(ctx, &definition))
		require.Contains(t, definition, "USING btree (node_pool_id)")
	}
	assertIndex()
	migrator := migrate.NewMigrator(database, migrations)
	for _, migration := range migrations.Sorted() {
		if migration.Name != "20261120120000" {
			continue
		}
		require.NoError(t, migration.Down(ctx, migrator, &migration))
		var exists bool
		require.NoError(t, database.NewRaw("SELECT to_regclass('public.nodes_node_pool_id_idx') IS NOT NULL").Scan(ctx, &exists))
		require.False(t, exists)
		for range 2 {
			require.NoError(t, migration.Up(ctx, migrator, &migration))
			assertIndex()
		}
		return
	}
	t.Fatal("nodes.node_pool_id index migration is not registered")
}
