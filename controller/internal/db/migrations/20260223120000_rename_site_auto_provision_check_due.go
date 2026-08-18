// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.Exec("ALTER TABLE sites RENAME COLUMN next_auto_provision_check_due TO next_reconcile_due"); err != nil {
			return fmt.Errorf("failed to rename sites.next_auto_provision_check_due: %w", err)
		}
		return nil
	},
		func(ctx context.Context, db *bun.DB) error {
			return fmt.Errorf("down migration not implemented")
		})
}
