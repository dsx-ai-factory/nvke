// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// clusters.cp_auto_scale_down_disabled is a cluster-level control-plane toggle,
// stored as a column alongside cp_desired_count and cp_max_surge rather than in
// the append-only config snapshot. When true the control plane is not scaled
// down automatically on a desiredCount decrease. A decrease used to be a no-op,
// so a cluster may already be running above its desiredCount; those are opted
// out here so the rollout does not shed control-plane nodes unprompted.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE clusters
				ADD COLUMN IF NOT EXISTS cp_auto_scale_down_disabled boolean NOT NULL DEFAULT false
		`); err != nil {
			return fmt.Errorf("add clusters.cp_auto_scale_down_disabled: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE clusters c
			SET cp_auto_scale_down_disabled = true
			WHERE c.cp_desired_count < (
				SELECT count(*) FROM nodes n
				WHERE n.cluster_id = c.id
					AND n.control_plane
					AND n.node_pool_id IS NULL
					AND n.conditions->'joined'->>'status' = 'True'
					AND COALESCE(n.conditions->'deleting'->>'status', '') <> 'True'
			)
		`); err != nil {
			return fmt.Errorf("backfill clusters.cp_auto_scale_down_disabled: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
