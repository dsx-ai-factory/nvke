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
	migrations.MustRegister(addClusterCertRenewalDueMigration, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}

func addClusterCertRenewalDueMigration(ctx context.Context, db *bun.DB) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := addClusterCertRenewalDue(ctx, tx); err != nil {
			return err
		}
		return backfillClusterCertRenewalDue(ctx, tx)
	})
}

func addClusterCertRenewalDue(ctx context.Context, db bun.IDB) error {
	if _, err := db.ExecContext(ctx, `ALTER TABLE clusters ADD COLUMN IF NOT EXISTS next_cert_renewal_due timestamptz`); err != nil {
		return fmt.Errorf("failed to add clusters.next_cert_renewal_due column: %w", err)
	}
	return nil
}

func backfillClusterCertRenewalDue(ctx context.Context, db bun.IDB) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE clusters
		SET next_cert_renewal_due = sites.next_cert_renewal_due
		FROM sites
		WHERE clusters.site_id = sites.id
	`); err != nil {
		return fmt.Errorf("failed to backfill clusters.next_cert_renewal_due: %w", err)
	}
	return nil
}
