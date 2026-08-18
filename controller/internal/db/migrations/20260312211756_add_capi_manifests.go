// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoinits // this is the typical way to register bun migrations
package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.Exec("ALTER TABLE clusters ADD COLUMN capi_infra_cluster jsonb NOT NULL DEFAULT '{}'::jsonb"); err != nil {
				return err
			}
			if _, err := tx.Exec("ALTER TABLE node_pools ADD COLUMN capi_infra_machine_template jsonb NOT NULL DEFAULT '{}'::jsonb"); err != nil {
				return err
			}
			if _, err := tx.Exec("ALTER TABLE node_sets ADD COLUMN capi_infra_machine_template jsonb NOT NULL DEFAULT '{}'::jsonb"); err != nil {
				return err
			}
			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
