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
		statements := []string{
			"ALTER TABLE clusters ADD COLUMN IF NOT EXISTS pod_cidr text",
			"ALTER TABLE clusters ADD COLUMN IF NOT EXISTS service_cidr text",
			"ALTER TABLE clusters ALTER COLUMN pod_cidr SET DEFAULT '192.168.0.0/16'",
			"ALTER TABLE clusters ALTER COLUMN service_cidr SET DEFAULT '10.96.0.0/12'",
			"UPDATE clusters SET pod_cidr = '192.168.0.0/16' WHERE pod_cidr IS NULL OR pod_cidr = ''",
			"UPDATE clusters SET service_cidr = '10.96.0.0/12' WHERE service_cidr IS NULL OR service_cidr = ''",
			"ALTER TABLE clusters ALTER COLUMN pod_cidr SET NOT NULL",
			"ALTER TABLE clusters ALTER COLUMN service_cidr SET NOT NULL",
		}
		for _, statement := range statements {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("failed to reconcile cluster network CIDR columns: %w", err)
			}
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
