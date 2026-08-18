// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // bun migrations register in init
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // Rollback is best-effort and harmless after a successful commit.

		if _, err := tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("capi_bootstrap_files jsonb not null default '[]'::jsonb").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_pools.capi_bootstrap_files column: %w", err)
		}
		if _, err := tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("capi_pre_kubeadm_commands text[] not null default '{}'").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_pools.capi_pre_kubeadm_commands column: %w", err)
		}
		if _, err := tx.NewAddColumn().
			Table("node_sets").
			ColumnExpr("capi_bootstrap_files jsonb not null default '[]'::jsonb").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_sets.capi_bootstrap_files column: %w", err)
		}
		if _, err := tx.NewAddColumn().
			Table("node_sets").
			ColumnExpr("capi_pre_kubeadm_commands text[] not null default '{}'").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_sets.capi_pre_kubeadm_commands column: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
