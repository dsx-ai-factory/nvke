// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
		if _, err := db.Exec("ALTER TABLE sites DROP COLUMN next_node_health_check_due"); err != nil {
			return fmt.Errorf("failed to drop sites.next_node_health_check_due: %w", err)
		}
		return nil
	},
		func(ctx context.Context, db *bun.DB) error {
			return fmt.Errorf("down migration not implemented")
		})
}
