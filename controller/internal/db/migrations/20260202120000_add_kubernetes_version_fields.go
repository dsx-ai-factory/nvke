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
			ColumnExpr("kubernetes_version text NOT NULL DEFAULT ''").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add node_pools.kubernetes_version column: %w", err)
		}

		_, err = tx.NewAddColumn().
			Table("node_sets").
			ColumnExpr("kubernetes_version text NOT NULL DEFAULT ''").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add node_sets.kubernetes_version column: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewDropColumn().
			Table("node_sets").
			Column("kubernetes_version").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop node_sets.kubernetes_version column: %w", err)
		}

		_, err = db.NewDropColumn().
			Table("node_pools").
			Column("kubernetes_version").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop node_pools.kubernetes_version column: %w", err)
		}

		return nil
	})
}
