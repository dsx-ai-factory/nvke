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
		// Add enable_cp_node_recycling column with default value of false
		// Default value of false ensures existing clusters don't automatically start upgrading
		_, err := db.NewAddColumn().
			Table("clusters").
			ColumnExpr("enable_cp_node_recycling boolean NOT NULL DEFAULT false").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add enable_cp_node_recycling column: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		// Rollback migration: drop enable_cp_node_recycling column
		_, err := db.NewDropColumn().
			Table("clusters").
			Column("enable_cp_node_recycling").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop enable_cp_node_recycling column: %w", err)
		}

		return nil
	})
}
