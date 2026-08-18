// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// First add the column with default false
		_, err := db.NewAddColumn().
			Table("node_pools").
			ColumnExpr("deprecated boolean NOT NULL DEFAULT false").
			Exec(ctx)
		if err != nil {
			return err
		}

		// Mark all snowflake node pools as deprecated with justification outlined in the description
		snowflakeDescription := "This node pool was created without the current NKE software stack " +
			"and was manually imported into CWE. Node add is disabled. " +
			"You must first create a new node pool to scale in available nodes."
		_, err = db.NewUpdate().
			Table("node_pools").
			Set("deprecated = ?", true).
			Set("description = ?", snowflakeDescription).
			Where("os_profile = ?", "snowflake").
			Exec(ctx)
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewDropColumn().
			Table("node_pools").
			Column("deprecated").
			Exec(ctx)
		return err
	})
}
