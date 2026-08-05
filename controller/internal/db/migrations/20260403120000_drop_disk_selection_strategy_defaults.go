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

//nolint:gochecknoinits // bun migrations register in init
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.Exec("ALTER TABLE node_pools ALTER COLUMN disk_selection_strategy DROP DEFAULT"); err != nil {
				return fmt.Errorf("failed to drop node_pools.disk_selection_strategy default: %w", err)
			}
			if _, err := tx.Exec("ALTER TABLE node_sets ALTER COLUMN disk_selection_strategy DROP DEFAULT"); err != nil {
				return fmt.Errorf("failed to drop node_sets.disk_selection_strategy default: %w", err)
			}

			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
