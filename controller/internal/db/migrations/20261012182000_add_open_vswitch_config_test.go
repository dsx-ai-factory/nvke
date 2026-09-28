// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestAddOpenVSwitchConfigRollsBackPartialFailure(t *testing.T) {
	ctx := context.Background()
	database := testutil.SetupDB(ctx, t)
	_, err := database.ExecContext(ctx, "CREATE TABLE node_pools (id text)")
	require.NoError(t, err)
	// The absent second table makes the migration fail after its first ALTER.
	require.Error(t, addOpenVSwitchConfig(ctx, database))
	_, err = database.ExecContext(ctx, "CREATE TABLE node_sets (id text)")
	require.NoError(t, err)
	// A retry can add both columns only if the first ALTER was rolled back.
	require.NoError(t, addOpenVSwitchConfig(ctx, database))
	for _, table := range []string{openVSwitchNodePoolsTable, openVSwitchNodeSetsTable} {
		_, err = database.ExecContext(ctx, "INSERT INTO ? (id) VALUES (?)", bun.Ident(table), "node")
		require.NoError(t, err)
		var config *string
		require.NoError(t, database.NewSelect().Table(table).Column("open_v_switch").Scan(ctx, &config))
		require.Nil(t, config, "existing configuration defaults to legacy behavior")
	}
}
