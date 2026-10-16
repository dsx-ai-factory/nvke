// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `ALTER TABLE node_pools
			ADD COLUMN machine_annotations jsonb NOT NULL DEFAULT '{}'::jsonb,
			ADD COLUMN machine_labels jsonb NOT NULL DEFAULT '{}'::jsonb`)
		if err != nil {
			return fmt.Errorf("add node pool machine metadata: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
