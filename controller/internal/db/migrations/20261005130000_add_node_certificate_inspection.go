// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // Bun migrations are registered during package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, database *bun.DB) error {
		_, err := database.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS certificate_inspection jsonb`)
		if err != nil {
			return fmt.Errorf("add nodes.certificate_inspection: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error { return fmt.Errorf("down migration not supported") })
}
