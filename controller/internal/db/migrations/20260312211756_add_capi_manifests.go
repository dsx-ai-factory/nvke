// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
