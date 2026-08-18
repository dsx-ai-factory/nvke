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
			SET conditions = conditions - 'kubetrustReady'
			WHERE conditions ? 'kubetrustReady'
		`)
		if err != nil {
			return fmt.Errorf("remove kubetrustReady from node conditions: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
