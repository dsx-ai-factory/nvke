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
		// Change vip_range column from text to text[] (array of text) and rename to vip_ranges
		if _, err := db.Exec("ALTER TABLE sites ALTER COLUMN vip_range TYPE text[] USING ARRAY[vip_range]"); err != nil {
			return err
		}
		if _, err := db.Exec("ALTER TABLE sites RENAME COLUMN vip_range TO vip_ranges"); err != nil {
			return err
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
