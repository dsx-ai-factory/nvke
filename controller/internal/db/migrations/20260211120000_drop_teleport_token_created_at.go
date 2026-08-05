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
		// Drop teleport_token_created_at column added in 20260204100000.
		// Token creation timestamps are now tracked in-memory by the
		// TeleportTokenController (resets on restart, which is safe since
		// token upsert is idempotent).
		if _, err := db.ExecContext(ctx,
			"ALTER TABLE nodes DROP COLUMN IF EXISTS teleport_token_created_at",
		); err != nil {
			return fmt.Errorf("failed to drop teleport_token_created_at column: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		// Rollback: re-add the column
		if _, err := db.NewAddColumn().
			Table("nodes").
			ColumnExpr("teleport_token_created_at timestamptz").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to re-add teleport_token_created_at column: %w", err)
		}

		return nil
	})
}
