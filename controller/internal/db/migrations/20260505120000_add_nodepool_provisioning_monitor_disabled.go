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
		_, err := db.ExecContext(ctx,
			"ALTER TABLE node_pools ADD COLUMN provisioning_monitor_disabled boolean NOT NULL DEFAULT false")
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
