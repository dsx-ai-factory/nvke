// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// clusters.cp_node_rebalance_disabled is a cluster-level control-plane toggle,
// stored as a column alongside cp_desired_count, cp_max_surge, and
// cp_auto_scale_down_disabled rather than in the append-only config snapshot. When
// false (the default for clusters created from here on) the controller rebalances
// the control plane onto more-preferred instance types and sheds the old node
// through the control-plane delete path. Rebalancing is new behavior, so every
// cluster that predates this column is opted out here; an operator turns it on
// per cluster. New rows take the column default and are opted in.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE clusters
				ADD COLUMN IF NOT EXISTS cp_node_rebalance_disabled boolean NOT NULL DEFAULT false
		`); err != nil {
			return fmt.Errorf("add clusters.cp_node_rebalance_disabled: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE clusters SET cp_node_rebalance_disabled = true
		`); err != nil {
			return fmt.Errorf("backfill clusters.cp_node_rebalance_disabled: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
