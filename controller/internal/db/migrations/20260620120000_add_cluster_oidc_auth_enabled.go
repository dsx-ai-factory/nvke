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
		_, err := db.NewAddColumn().
			Table("clusters").
			ColumnExpr("oidc_auth_enabled boolean NOT NULL DEFAULT false").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add clusters.oidc_auth_enabled column: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
