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
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS vip_allocations`); err != nil {
				return fmt.Errorf("drop vip_allocations table: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS sites`); err != nil {
				return fmt.Errorf("drop sites table: %w", err)
			}
			return nil
		})
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
