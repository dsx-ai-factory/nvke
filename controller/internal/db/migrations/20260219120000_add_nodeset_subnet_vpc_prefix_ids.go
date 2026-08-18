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
		_, err := db.NewAddColumn().
			Table("node_sets").
			ColumnExpr("subnet_ids uuid[] NOT NULL DEFAULT '{}'").
			Exec(ctx)
		if err != nil {
			return err
		}
		_, err = db.NewAddColumn().
			Table("node_sets").
			ColumnExpr("vpc_prefix_ids uuid[] NOT NULL DEFAULT '{}'").
			Exec(ctx)
		if err != nil {
			return err
		}
		// Backfill current NodeSets from their NodePool so we don't spuriously create new NodeSets.
		_, err = db.ExecContext(ctx,
			`UPDATE node_sets SET subnet_ids = np.subnet_ids, vpc_prefix_ids = np.vpc_prefix_ids
			 FROM node_pools np WHERE np.current_node_set_id = node_sets.id`)
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
