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

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewAddColumn().
			Table("clusters").
			ColumnExpr("teleport_enabled bool not null default false").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add teleport_enabled column to clusters: %w", err)
		}

		if _, err := db.NewAddColumn().
			Table("node_pools").
			ColumnExpr("teleport_enabled bool not null default false").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add teleport_enabled column to node_pools: %w", err)
		}

		if _, err := db.NewAddColumn().
			Table("node_sets").
			ColumnExpr("teleport_enabled bool not null default false").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add teleport_enabled column to node_sets: %w", err)
		}

		if _, err := db.NewAddColumn().
			Table("nodes").
			ColumnExpr("teleport_token_created_at timestamptz").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add teleport_token_created_at column to nodes: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewDropColumn().
			Table("nodes").
			Column("teleport_token_created_at").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to drop teleport_token_created_at column from nodes: %w", err)
		}

		if _, err := db.NewDropColumn().
			Table("node_sets").
			Column("teleport_enabled").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to drop teleport_enabled column from node_sets: %w", err)
		}

		if _, err := db.NewDropColumn().
			Table("node_pools").
			Column("teleport_enabled").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to drop teleport_enabled column from node_pools: %w", err)
		}

		if _, err := db.NewDropColumn().
			Table("clusters").
			Column("teleport_enabled").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to drop teleport_enabled column from clusters: %w", err)
		}

		return nil
	})
}
