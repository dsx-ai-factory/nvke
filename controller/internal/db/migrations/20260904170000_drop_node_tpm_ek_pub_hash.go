// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP COLUMN IF EXISTS tpm_ek_pub_hash`); err != nil {
			return fmt.Errorf("failed to drop nodes.tpm_ek_pub_hash: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
