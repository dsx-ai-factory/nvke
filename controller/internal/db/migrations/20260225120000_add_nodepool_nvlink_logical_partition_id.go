// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewAddColumn().
			Table("node_pools").
			ColumnExpr("nv_link_logical_partition_id uuid").
			Exec(ctx); err != nil {
			return err
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
