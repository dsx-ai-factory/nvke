// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewAddColumn().
			Table("node_pools").
			ColumnExpr("description TEXT"). // Add as TEXT, nullable by default
			Exec(ctx)
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewDropColumn().
			Table("node_pools").
			Column("description").
			Exec(ctx)
		return err
	})
}
