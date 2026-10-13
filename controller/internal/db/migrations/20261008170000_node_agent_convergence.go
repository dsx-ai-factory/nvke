// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() { //nolint:gochecknoinits // Bun registers migrations at package initialization.
	migrations.MustRegister(func(ctx context.Context, database *bun.DB) error {
		return database.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			for _, statement := range []string{
				// Keep one selected upgrade after a restart, even while its agent is unreachable.
				// Track accepted renewal work so agent restarts cannot interrupt it.
				"ALTER TABLE clusters ADD COLUMN node_agent_upgrade jsonb, ADD COLUMN certificate_renewal jsonb",
				// Preserve the first provisioning version and method across retries and configuration changes.
				"ALTER TABLE nodes ADD COLUMN node_agent_lifecycle jsonb",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("adding node-agent convergence state: %w", err)
				}
			}
			return nil
		})
	}, func(context.Context, *bun.DB) error {
		// Dropping these columns would discard active upgrade and renewal coordination.
		return fmt.Errorf("down migration not supported")
	})
}
