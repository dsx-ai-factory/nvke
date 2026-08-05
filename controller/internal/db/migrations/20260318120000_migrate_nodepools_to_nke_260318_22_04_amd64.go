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

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck // Rollback is safe to ignore: it's a no-op after successful Commit and we handle tx errors at Commit

		// Update only the NodePool's desired OS profile. The NodePoolController will
		// detect that the current NodeSet still references the old profile, create a
		// new current NodeSet for the replacement profile, and let old nodes recycle
		// through the normal non-current/deprecated NodeSet flow.
		_, err = tx.NewUpdate().
			Table("node_pools").
			Set("os_profile = ?", "nke-260318-22.04-amd64").
			Where("os_profile = ?", "nke-260303-22.04-amd64").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to migrate node_pools.os_profile: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
