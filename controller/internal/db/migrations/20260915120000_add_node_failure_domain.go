// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoinits // this is the typical way to register bun migrations
package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// init registers the node failure-domain migration.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS failure_domain text NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add nodes.failure_domain: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP COLUMN IF EXISTS failure_domain`); err != nil {
			return fmt.Errorf("drop nodes.failure_domain: %w", err)
		}
		return nil
	})
}
