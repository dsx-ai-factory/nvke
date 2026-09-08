// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// TestMigrateControlPlaneToControllerFlagOffIsNoop verifies the sweep short-circuits
// before touching the database when the site has not opted in. The nil *bun.DB would
// panic if the function dereferenced it, so a clean nil return proves the guard runs
// first. This is the only branch that can be exercised without a live Postgres
// instance; the SQL body is validated in staging against a real database.
func TestMigrateControlPlaneToControllerFlagOffIsNoop(t *testing.T) {
	require.NotPanics(t, func() {
		err := db.MigrateControlPlaneToController(context.Background(), nil, false)
		require.NoError(t, err)
	})
}
