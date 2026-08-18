// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // Rollback is safe to ignore: it's a no-op after successful Commit and we handle tx errors at Commit

		_, err = tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("disk_selection_strategy text NOT NULL DEFAULT 'smallest'").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add node_pools.disk_selection_strategy column: %w", err)
		}

		_, err = tx.NewAddColumn().
			Table("node_sets").
			ColumnExpr("disk_selection_strategy text NOT NULL DEFAULT 'smallest'").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add node_sets.disk_selection_strategy column: %w", err)
		}

		// Backfill node_pools/node_sets.disk_selection_strategy for osmo profiles
		// There was only ever one osmo profile (which is now deprecated), so we can just do a dumb string comparison.
		_, err = tx.NewUpdate().
			Table("node_pools").
			Set("disk_selection_strategy = ?", "largest").
			Where("os_profile = ?", "nke-1.31.12-251029-osmo").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to backfill node_pools.disk_selection_strategy for osmo profiles: %w", err)
		}

		_, err = tx.NewUpdate().
			Table("node_sets").
			Set("disk_selection_strategy = ?", "largest").
			Where("os_profile = ?", "nke-1.31.12-251029-osmo").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to backfill node_sets.disk_selection_strategy for osmo profiles: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
