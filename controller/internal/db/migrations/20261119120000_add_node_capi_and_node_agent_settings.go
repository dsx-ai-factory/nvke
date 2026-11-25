// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS capi jsonb`); err != nil {
			return fmt.Errorf("add nodes.capi: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS node_agent_token_source text NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add nodes.node_agent_token_source: %w", err)
		}
		// node_agent holds the provider-independent node-agent settings a pool
		// pins onto its nodes, so later settings need no further migration.
		for _, table := range []string{"node_pools", "node_sets"} {
			stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS node_agent jsonb`, table)
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("add %s.node_agent: %w", table, err)
			}
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
