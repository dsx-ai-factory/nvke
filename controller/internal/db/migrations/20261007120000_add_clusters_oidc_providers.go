// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// clusters.oidc_providers holds the fleet-wide OIDC providers a cluster was
// created with. NULL means none were pinned, which is every cluster created
// before this column existed.
//
//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `
			ALTER TABLE clusters
				ADD COLUMN IF NOT EXISTS oidc_providers JSONB
				CONSTRAINT clusters_oidc_providers_array
				CHECK (oidc_providers IS NULL OR jsonb_typeof(oidc_providers) = 'array')
		`)
		if err != nil {
			return fmt.Errorf("add clusters.oidc_providers: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
