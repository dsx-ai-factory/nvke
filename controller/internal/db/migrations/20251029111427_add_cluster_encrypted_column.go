// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	const (
		tableName = "clusters"
	)
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// First add the column with default false
		_, err := db.NewAddColumn().
			Table(tableName).
			ColumnExpr("etcd_encrypted boolean NOT NULL DEFAULT false").
			Exec(ctx)
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewDropColumn().
			Table(tableName).
			Column("etcd_encrypted").
			Exec(ctx)
		return err
	})
}
