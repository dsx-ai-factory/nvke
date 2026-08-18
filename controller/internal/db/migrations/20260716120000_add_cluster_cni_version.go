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
		if _, err := db.ExecContext(ctx, `ALTER TABLE clusters ADD COLUMN IF NOT EXISTS cni_version text`); err != nil {
			return fmt.Errorf("failed to add clusters.cni_version column: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE clusters
			SET cni_version = CASE cni_plugin
				WHEN 'calico' THEN '3.30.1'
				WHEN 'ovn-kubernetes' THEN '1.4.0'
				ELSE cni_version
			END
			WHERE cni_version IS NULL OR btrim(cni_version) = ''
		`); err != nil {
			return fmt.Errorf("failed to backfill clusters.cni_version: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE clusters DROP COLUMN IF EXISTS cni_version`); err != nil {
			return fmt.Errorf("failed to drop clusters.cni_version column: %w", err)
		}
		return nil
	})
}
