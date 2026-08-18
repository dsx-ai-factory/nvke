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
		// Empty values retain kubeadm-patches' existing default.
		for _, table := range []string{"node_pools", "node_sets"} {
			if _, err := db.NewAddColumn().
				Table(table).
				ColumnExpr("kube_reserved_memory text NOT NULL DEFAULT ''").
				Exec(ctx); err != nil {
				return err
			}
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
