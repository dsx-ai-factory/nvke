// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
		// Change subnet_id column from uuid to uuid[] (array of uuid) and rename to subnet_ids
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.Exec("ALTER TABLE node_pools ALTER COLUMN subnet_id TYPE uuid[] USING ARRAY[subnet_id]"); err != nil {
				return err
			}
			if _, err := tx.Exec("ALTER TABLE node_pools RENAME COLUMN subnet_id TO subnet_ids"); err != nil {
				return err
			}
			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
