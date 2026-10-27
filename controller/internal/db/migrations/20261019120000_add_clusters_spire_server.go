// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// clusters.spire_server holds the SPIRE server last requested for a cluster's
// nodes through PATCH /clusters/{clusterID}/spire-server. NULL means none was
// requested.
//
//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `
			ALTER TABLE clusters
				ADD COLUMN IF NOT EXISTS spire_server JSONB
				CONSTRAINT clusters_spire_server_object
				CHECK (spire_server IS NULL OR jsonb_typeof(spire_server) = 'object')
		`)
		if err != nil {
			return fmt.Errorf("add clusters.spire_server: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
