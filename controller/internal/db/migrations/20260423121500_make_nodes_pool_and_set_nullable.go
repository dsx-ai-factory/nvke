// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		stmts := []string{
			`ALTER TABLE nodes ALTER COLUMN node_pool_id DROP NOT NULL`,
			`ALTER TABLE nodes ALTER COLUMN node_set_id DROP NOT NULL`,
		}
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("exec %q: %w", stmt, err)
			}
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		var nullCount int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM nodes
			WHERE node_pool_id IS NULL OR node_set_id IS NULL
		`).Scan(&nullCount); err != nil {
			return fmt.Errorf("count NULL node_pool_id/node_set_id: %w", err)
		}
		if nullCount > 0 {
			return fmt.Errorf("cannot restore NOT NULL on nodes.node_pool_id/node_set_id: found %d rows with NULL values", nullCount)
		}

		stmts := []string{
			`ALTER TABLE nodes ALTER COLUMN node_pool_id SET NOT NULL`,
			`ALTER TABLE nodes ALTER COLUMN node_set_id SET NOT NULL`,
		}
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("exec %q: %w", stmt, err)
			}
		}
		return nil
	})
}
