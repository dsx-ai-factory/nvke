// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
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
