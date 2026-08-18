// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	cwedb "github.com/NVIDIA/nke/controller/internal/db"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewAddColumn().
			Table("clusters").
			ColumnExpr("control_plane_config jsonb NOT NULL DEFAULT '{}'::jsonb").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add control_plane_config column: %w", err)
		}

		// Pin the SELECT to the columns that existed on node_pools at the time of this migration.
		// Bun would otherwise emit a SELECT for every column on the live cwedb.NodePool struct,
		// which fails on databases that have not yet applied later migrations adding new columns.
		// Developer Note: The normal pattern for this is to create a local Model struct that only includes the columns needed for the migration.
		// But this change needed to be made after the migration merged as it was breaking future migrations.
		var pools []cwedb.NodePool
		err = db.NewSelect().
			Model(&pools).
			Column(
				"id",
				"cluster_id",
				"name",
				"node_name_prefix",
				"subnet_ids",
				"vpc_prefix_ids",
				"instance_type_id",
				"description",
				"infiniband_partition_id",
				"nv_link_logical_partition_id",
				"ssh_key_group_i_ds",
				"os_profile",
				"config_profile",
				"system_reserved_memory",
				"control_plane",
				"node_labels",
				"conditions",
				"created_at",
				"updated_at",
				"kubernetes_version",
				"desired_count",
				"auto_provision_disabled",
				"max_surge",
				"current_node_set_id",
				"auto_mode_disabled",
				"recycle_budget",
				"teleport_enabled",
				"disk_selection_strategy",
				"capi_infra_machine_template",
				"capi_bootstrap_files",
				"capi_pre_kubeadm_commands",
				"capi_spire_attestation_method",
			).
			Where("control_plane = ?", true).
			Order("cluster_id ASC").
			Order("created_at ASC").
			Scan(ctx)
		if err != nil {
			return fmt.Errorf("failed to list control plane node pools: %w", err)
		}

		byCluster := make(map[uuid.UUID][]cwedb.NodePool)
		for i := range pools {
			p := pools[i]
			byCluster[p.ClusterID] = append(byCluster[p.ClusterID], p)
		}

		for clusterID, cPools := range byCluster {
			cfg := cwedb.MergeControlPlaneConfigFromNodePools(cPools)
			if cfg == nil {
				continue
			}

			_, err = db.NewUpdate().
				Model(&cwedb.Cluster{ID: clusterID, ControlPlaneConfig: *cfg}).
				Column("control_plane_config").
				WherePK().
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("failed to backfill control_plane_config for cluster %s: %w", clusterID, err)
			}
		}

		stmts := []string{
			`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cluster_id uuid`,
			`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS control_plane boolean NOT NULL DEFAULT false`,
			`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS instance_type_id uuid`,
		}
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("exec %q: %w", stmt, err)
			}
		}

		if _, err := db.ExecContext(ctx, `
			UPDATE nodes n
			SET cluster_id = np.cluster_id
			FROM node_pools np
			WHERE n.node_pool_id = np.id AND n.cluster_id IS NULL
		`); err != nil {
			return fmt.Errorf("backfill nodes.cluster_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE nodes n
			SET control_plane = np.control_plane
			FROM node_pools np
			WHERE n.node_pool_id = np.id
		`); err != nil {
			return fmt.Errorf("backfill nodes.control_plane: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE nodes n
			SET instance_type_id = np.instance_type_id
			FROM node_pools np
			WHERE n.node_pool_id = np.id AND n.instance_type_id IS NULL
		`); err != nil {
			return fmt.Errorf("backfill nodes.instance_type_id: %w", err)
		}

		var orphanedNodes int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM nodes n
			LEFT JOIN node_pools np ON np.id = n.node_pool_id
			WHERE np.id IS NULL
		`).Scan(&orphanedNodes); err != nil {
			return fmt.Errorf("count orphaned nodes: %w", err)
		}
		if orphanedNodes > 0 {
			return fmt.Errorf("cannot enforce nodes.cluster_id/instance_type_id NOT NULL: found %d orphaned nodes without matching node_pool_id", orphanedNodes)
		}

		var nullNodePoolInstanceType int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM node_pools
			WHERE instance_type_id IS NULL
		`).Scan(&nullNodePoolInstanceType); err != nil {
			return fmt.Errorf("count node_pools with NULL instance_type_id: %w", err)
		}
		if nullNodePoolInstanceType > 0 {
			return fmt.Errorf("cannot enforce nodes.instance_type_id NOT NULL: found %d node_pools with NULL instance_type_id", nullNodePoolInstanceType)
		}

		var nullNodeColumns int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM nodes
			WHERE cluster_id IS NULL OR instance_type_id IS NULL
		`).Scan(&nullNodeColumns); err != nil {
			return fmt.Errorf("count nodes with NULL cluster_id/instance_type_id: %w", err)
		}
		if nullNodeColumns > 0 {
			return fmt.Errorf("cannot enforce nodes.cluster_id/instance_type_id NOT NULL: found %d rows with NULL values after backfill", nullNodeColumns)
		}

		if _, err := db.ExecContext(ctx, `
			DO $$
			BEGIN
				IF NOT EXISTS (
					SELECT 1 FROM pg_constraint WHERE conname = 'fk_nodes_cluster_id'
				) THEN
					ALTER TABLE nodes ADD CONSTRAINT fk_nodes_cluster_id
					FOREIGN KEY (cluster_id) REFERENCES clusters(id);
				END IF;
			END
			$$;
		`); err != nil {
			return fmt.Errorf("add fk_nodes_cluster_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS nodes_cluster_id_idx ON nodes (cluster_id)`); err != nil {
			return fmt.Errorf("create nodes_cluster_id_idx: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ALTER COLUMN cluster_id SET NOT NULL`); err != nil {
			return fmt.Errorf("set NOT NULL on nodes.cluster_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ALTER COLUMN instance_type_id SET NOT NULL`); err != nil {
			return fmt.Errorf("set NOT NULL on nodes.instance_type_id: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS nodes_cluster_id_idx`); err != nil {
			return fmt.Errorf("failed to drop nodes_cluster_id_idx: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP CONSTRAINT IF EXISTS fk_nodes_cluster_id`); err != nil {
			return fmt.Errorf("failed to drop fk_nodes_cluster_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP COLUMN IF EXISTS instance_type_id`); err != nil {
			return fmt.Errorf("failed to drop nodes.instance_type_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP COLUMN IF EXISTS control_plane`); err != nil {
			return fmt.Errorf("failed to drop nodes.control_plane: %w", err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes DROP COLUMN IF EXISTS cluster_id`); err != nil {
			return fmt.Errorf("failed to drop nodes.cluster_id: %w", err)
		}

		_, err := db.NewDropColumn().
			Table("clusters").
			Column("control_plane_config").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop control_plane_config column: %w", err)
		}
		return nil
	})
}
