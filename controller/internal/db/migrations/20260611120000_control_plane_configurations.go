// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:goconst // Migrations are immutable historical snapshots; extracting shared constants would couple applied migrations to values that change later.
package migrations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	cwedb "github.com/NVIDIA/nke/controller/internal/db"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS control_plane_configurations (
				id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				cluster_id UUID NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
				config JSONB NOT NULL,
				change_reason TEXT,
				created_by TEXT,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now()
			)
		`)
		if err != nil {
			return fmt.Errorf("create control_plane_configurations: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			CREATE INDEX IF NOT EXISTS control_plane_configurations_cluster_id_idx
			ON control_plane_configurations (cluster_id)
		`); err != nil {
			return fmt.Errorf("create control_plane_configurations_cluster_id_idx: %w", err)
		}

		clusterColumns := []string{
			`ALTER TABLE clusters ADD COLUMN IF NOT EXISTS configuration_id uuid`,
			`ALTER TABLE clusters ADD COLUMN IF NOT EXISTS cp_desired_count integer NOT NULL DEFAULT 0`,
			`ALTER TABLE clusters ADD COLUMN IF NOT EXISTS cp_max_surge integer NOT NULL DEFAULT 0`,
			`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS configuration_id uuid`,
		}
		for _, stmt := range clusterColumns {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("exec %q: %w", stmt, err)
			}
		}

		type clusterRow struct {
			ID                 uuid.UUID       `bun:"id"`
			ControlPlaneConfig json.RawMessage `bun:"control_plane_config"`
		}
		var clusters []clusterRow
		if err := db.NewSelect().
			TableExpr("clusters").
			Column("id", "control_plane_config").
			Scan(ctx, &clusters); err != nil {
			return fmt.Errorf("list clusters for control plane configuration backfill: %w", err)
		}

		for _, cluster := range clusters {
			cfg, err := cwedb.LegacyControlPlaneConfigFromJSON(cluster.ControlPlaneConfig)
			if err != nil {
				return fmt.Errorf("parse control_plane_config for cluster %s: %w", cluster.ID, err)
			}
			if cfg.Equal(cwedb.ControlPlaneConfig{}) {
				continue
			}

			row := &cwedb.ControlPlaneConfiguration{
				ClusterID:    cluster.ID,
				Config:       cwedb.ConfigSpecFromFull(cfg),
				ChangeReason: "migration from clusters.control_plane_config",
			}
			if _, err := db.NewInsert().Model(row).Exec(ctx); err != nil {
				return fmt.Errorf("insert control plane configuration for cluster %s: %w", cluster.ID, err)
			}

			_, err = db.NewUpdate().
				Model(&cwedb.Cluster{
					ID: cluster.ID,
					ControlPlaneConfig: cwedb.ControlPlaneConfig{
						ConfigurationID: row.ID,
						DesiredCount:    cfg.DesiredCount,
						MaxSurge:        cfg.MaxSurge,
					},
				}).
				Column("configuration_id", "cp_desired_count", "cp_max_surge").
				WherePK().
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("update cluster %s control plane columns: %w", cluster.ID, err)
			}
		}

		if _, err := db.ExecContext(ctx, `
			DO $$
			BEGIN
				IF NOT EXISTS (
					SELECT 1 FROM pg_constraint WHERE conname = 'fk_clusters_configuration_id'
				) THEN
					ALTER TABLE clusters ADD CONSTRAINT fk_clusters_configuration_id
					FOREIGN KEY (configuration_id) REFERENCES control_plane_configurations(id);
				END IF;
			END
			$$;
		`); err != nil {
			return fmt.Errorf("add fk_clusters_configuration_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			DO $$
			BEGIN
				IF NOT EXISTS (
					SELECT 1 FROM pg_constraint WHERE conname = 'fk_nodes_configuration_id'
				) THEN
					ALTER TABLE nodes ADD CONSTRAINT fk_nodes_configuration_id
					FOREIGN KEY (configuration_id) REFERENCES control_plane_configurations(id);
				END IF;
			END
			$$;
		`); err != nil {
			return fmt.Errorf("add fk_nodes_configuration_id: %w", err)
		}

		// FIXME: https://jirasw.nvidia.com/browse/NKX-13236
		// Expand/contract safety: keep legacy clusters.control_plane_config in this
		// release; drop it in a follow-up migration after rollout completion.
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewAddColumn().
			Table("clusters").
			ColumnExpr("control_plane_config jsonb NOT NULL DEFAULT '{}'::jsonb").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("restore control_plane_config column: %w", err)
		}

		type clusterRow struct {
			ID              uuid.UUID `bun:"id"`
			ConfigurationID uuid.UUID `bun:"configuration_id"`
			CPDesiredCount  int       `bun:"cp_desired_count"`
			CPMaxSurge      int       `bun:"cp_max_surge"`
		}
		var clusters []clusterRow
		if err := db.NewSelect().
			TableExpr("clusters").
			Column("id", "configuration_id", "cp_desired_count", "cp_max_surge").
			Where("configuration_id IS NOT NULL").
			Scan(ctx, &clusters); err != nil {
			return fmt.Errorf("list clusters for rollback backfill: %w", err)
		}

		for _, cluster := range clusters {
			var row cwedb.ControlPlaneConfiguration
			if err := db.NewSelect().Model(&row).Where("id = ?", cluster.ConfigurationID).Scan(ctx); err != nil {
				return fmt.Errorf("load configuration %s: %w", cluster.ConfigurationID, err)
			}
			cfg := cwedb.BuildControlPlaneConfig(cwedb.Cluster{
				ControlPlaneConfig: cwedb.ControlPlaneConfig{
					ConfigurationID: cluster.ConfigurationID,
					DesiredCount:    cluster.CPDesiredCount,
					MaxSurge:        cluster.CPMaxSurge,
				},
			}, row.Config, row.CreatedAt)
			raw, err := json.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("marshal control_plane_config for cluster %s: %w", cluster.ID, err)
			}
			if _, err := db.NewUpdate().
				TableExpr("clusters").
				Set("control_plane_config = ?", raw).
				Where("id = ?", cluster.ID).
				Exec(ctx); err != nil {
				return fmt.Errorf("restore control_plane_config for cluster %s: %w", cluster.ID, err)
			}
		}

		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP CONSTRAINT IF EXISTS fk_nodes_configuration_id`); err != nil {
			return fmt.Errorf("drop fk_nodes_configuration_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE clusters DROP CONSTRAINT IF EXISTS fk_clusters_configuration_id`); err != nil {
			return fmt.Errorf("drop fk_clusters_configuration_id: %w", err)
		}

		dropColumns := []struct {
			table  string
			column string
		}{
			{"nodes", "configuration_id"},
			{"clusters", "configuration_id"},
			{"clusters", "cp_desired_count"},
			{"clusters", "cp_max_surge"},
		}
		for _, col := range dropColumns {
			if _, err := db.NewDropColumn().Table(col.table).Column(col.column).Exec(ctx); err != nil {
				return fmt.Errorf("drop %s.%s: %w", col.table, col.column, err)
			}
		}

		_, err = db.NewDropTable().
			TableExpr("control_plane_configurations").
			IfExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("drop control_plane_configurations: %w", err)
		}
		return nil
	})
}
