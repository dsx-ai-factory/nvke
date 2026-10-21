// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoinits // this is the typical way to register bun migrations
package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE clusters ADD COLUMN IF NOT EXISTS launch_layer jsonb`); err != nil {
				return fmt.Errorf("failed to add clusters.launch_layer column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE node_pools ADD COLUMN IF NOT EXISTS launch_layer jsonb`); err != nil {
				return fmt.Errorf("failed to add node_pools.launch_layer column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE node_sets ADD COLUMN IF NOT EXISTS launch_layer jsonb`); err != nil {
				return fmt.Errorf("failed to add node_sets.launch_layer column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS launch_layer jsonb`); err != nil {
				return fmt.Errorf("failed to add nodes.launch_layer column: %w", err)
			}
			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
