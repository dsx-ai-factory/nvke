// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// nodes.capi holds per-node state for infra (CAPI) provider nodes, such as
// whether the machine is a KubeVirt VM and the address the controller dials
// its node agent at. Existing rows keep NULL, which means a bare-metal node
// dialed at its machine address.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS capi jsonb`); err != nil {
			return fmt.Errorf("add nodes.capi: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
