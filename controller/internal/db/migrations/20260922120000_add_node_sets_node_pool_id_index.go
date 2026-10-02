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
		if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS node_sets_node_pool_id_idx ON node_sets (node_pool_id)`); err != nil {
			return fmt.Errorf("create node_sets_node_pool_id_idx: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS node_sets_node_pool_id_idx`); err != nil {
			return fmt.Errorf("drop node_sets_node_pool_id_idx: %w", err)
		}
		return nil
	})
}
