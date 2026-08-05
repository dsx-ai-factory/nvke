// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
		// Add max_surge column with default value of 0
		// Default value of 0 provides conservative upgrade (delete then provision - no surge)
		_, err := db.NewAddColumn().
			Table("node_pools").
			ColumnExpr("max_surge integer NOT NULL DEFAULT 0").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add max_surge column: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		// Rollback migration: drop max_surge column
		_, err := db.NewDropColumn().
			Table("node_pools").
			Column("max_surge").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop max_surge column: %w", err)
		}

		return nil
	})
}
