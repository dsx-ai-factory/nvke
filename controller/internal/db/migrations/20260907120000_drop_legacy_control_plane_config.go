// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// This is the contract half of the expand/contract that introduced
// control_plane_configurations (20260611120000). That migration backfilled the
// new control_plane_configurations table plus the clusters.configuration_id,
// cp_desired_count and cp_max_surge columns from the legacy
// clusters.control_plane_config JSONB column, then deliberately kept the legacy
// column for one release.
//
// By the time this migration runs, no code path reads or writes
// clusters.control_plane_config: cwedb.Cluster maps ControlPlaneConfig via
// bun:",embed" to configuration_id/cp_desired_count/cp_max_surge, all reads go
// through control_plane_configurations, and newly created clusters only ever
// populate configuration_id (never the legacy column, which held its '{}'
// default). Dropping the column is therefore safe.
//
// This drop is intentionally one-way: there is no rollback. The legacy column
// is dead storage and we do not want it recreated on a downgrade, so the down
// migration is a no-op.
//
//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE clusters DROP COLUMN IF EXISTS control_plane_config`); err != nil {
			return fmt.Errorf("drop clusters.control_plane_config: %w", err)
		}
		return nil
	}, func(_ context.Context, _ *bun.DB) error {
		// Intentionally irreversible: once the legacy column is dropped we do not
		// recreate it.
		return nil
	})
}
