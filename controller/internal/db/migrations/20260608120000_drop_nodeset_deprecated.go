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
		if _, err := db.NewDropColumn().
			Table("node_sets").
			Column("deprecated").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to drop node_sets.deprecated column: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
