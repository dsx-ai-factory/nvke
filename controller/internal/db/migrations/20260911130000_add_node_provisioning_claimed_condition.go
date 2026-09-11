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
		_, err := db.ExecContext(ctx, `
			UPDATE nodes
			SET conditions = jsonb_set(
				conditions,
				'{provisioningClaimed}',
				CASE
					WHEN conditions->'pending'->>'status' = 'True'
						AND COALESCE(conditions->'provisioning'->>'status', '') != 'True'
					THEN '{"status":"True","message":"Provisioning workflow claimed node"}'::jsonb
					ELSE '{"status":"False","message":"Provisioning workflow has not claimed node"}'::jsonb
				END
			)
			WHERE NOT conditions ? 'provisioningClaimed'
		`)
		if err != nil {
			return fmt.Errorf("add provisioningClaimed node condition: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
