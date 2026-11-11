// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// nodes.failure_domain_unavailable_since and nodes.failure_domains_tried track a
// pending control-plane node being moved between failure domains.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE nodes
				ADD COLUMN IF NOT EXISTS failure_domain_unavailable_since timestamptz,
				ADD COLUMN IF NOT EXISTS failure_domains_tried text[]
		`); err != nil {
			return fmt.Errorf("add nodes failure-domain fallback columns: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
