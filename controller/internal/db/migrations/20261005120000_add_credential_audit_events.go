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
			_, err := tx.ExecContext(ctx, `CREATE TABLE credential_audit_events (
				event_id text PRIMARY KEY CHECK (event_id ~ '^[a-f0-9]{64}$'),
				observed_at timestamptz NOT NULL,
				recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
				event jsonb NOT NULL CHECK (event->>'schema_version' = 'nke.credential-audit.v1')
			)`)
			if err != nil {
				return fmt.Errorf("create credential audit ledger: %w", err)
			}
			for _, statement := range []string{
				`CREATE INDEX credential_audit_events_observed_at_idx ON credential_audit_events (observed_at)`,
				`CREATE INDEX credential_audit_events_cluster_observed_at_idx ON credential_audit_events ((event #>> '{target,cluster_id}'), observed_at)`,
				`CREATE INDEX credential_audit_events_workflow_idx ON credential_audit_events ((event #>> '{correlation,workflow_hash}'))`,
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("index credential audit ledger: %w", err)
				}
			}
			return nil
		})
	}, func(ctx context.Context, database *bun.DB) error {
		_, err := database.ExecContext(ctx, `DROP TABLE credential_audit_events`)
		return err
	})
}
