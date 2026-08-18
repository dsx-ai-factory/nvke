// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.Exec("ALTER TABLE node_pools ADD COLUMN auto_mode_disabled boolean NOT NULL DEFAULT false"); err != nil {
				return err
			}
			if _, err := tx.Exec("ALTER TABLE node_pools ADD COLUMN recycle_budget integer NOT NULL DEFAULT 0"); err != nil {
				return err
			}
			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.Exec("ALTER TABLE node_pools DROP COLUMN auto_mode_disabled"); err != nil {
				return err
			}
			if _, err := tx.Exec("ALTER TABLE node_pools DROP COLUMN recycle_budget"); err != nil {
				return err
			}
			return nil
		})
	})
}
