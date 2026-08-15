// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

//nolint:gochecknoinits // this is the typical way to register bun migrations
package migrations

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE clusters ADD COLUMN IF NOT EXISTS provider_type text`); err != nil {
				return fmt.Errorf("failed to add clusters.provider_type column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE clusters ADD COLUMN IF NOT EXISTS nico jsonb`); err != nil {
				return fmt.Errorf("failed to add clusters.nico column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE clusters ADD COLUMN IF NOT EXISTS capi jsonb`); err != nil {
				return fmt.Errorf("failed to add clusters.capi column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE node_pools ADD COLUMN IF NOT EXISTS nico jsonb`); err != nil {
				return fmt.Errorf("failed to add node_pools.nico column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE node_pools ADD COLUMN IF NOT EXISTS capi jsonb`); err != nil {
				return fmt.Errorf("failed to add node_pools.capi column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE node_sets ADD COLUMN IF NOT EXISTS nico jsonb`); err != nil {
				return fmt.Errorf("failed to add node_sets.nico column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE node_sets ADD COLUMN IF NOT EXISTS capi jsonb`); err != nil {
				return fmt.Errorf("failed to add node_sets.capi column: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS nico jsonb`); err != nil {
				return fmt.Errorf("failed to add nodes.nico column: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				UPDATE clusters
				SET
					provider_type = CASE
						WHEN site_id = ? THEN 'capi'
						ELSE 'nico'
					END,
					nico = CASE
						WHEN site_id = ? THEN NULL
						ELSE jsonb_build_object(
							'org_id', org_id,
							'site_id', site_id,
							'vpc_id', vpc_id
						)
					END,
					capi = CASE
						WHEN site_id = ? THEN jsonb_build_object(
							'infra_cluster', capi_infra_cluster
						)
						ELSE NULL
					END
			`, uuid.Nil, uuid.Nil, uuid.Nil); err != nil {
				return fmt.Errorf("failed to backfill cluster provider payloads: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE clusters ALTER COLUMN provider_type SET NOT NULL`); err != nil {
				return fmt.Errorf("failed to set clusters.provider_type not null: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE node_pools np
				SET
					nico = CASE
						WHEN c.provider_type = 'nico' THEN jsonb_build_object(
							'subnet_ids', to_jsonb(np.subnet_ids),
							'vpc_prefix_ids', to_jsonb(np.vpc_prefix_ids),
							'instance_type_id', np.instance_type_id,
							'infiniband_partition_id', np.infiniband_partition_id,
							'nvlink_logical_partition_id', np.nv_link_logical_partition_id,
							'ssh_key_group_ids', to_jsonb(np.ssh_key_group_i_ds),
							'os_profile', np.os_profile,
							'config_profile', np.config_profile,
							'disk_selection_strategy', np.disk_selection_strategy
						)
						ELSE NULL
					END,
					capi = CASE
						WHEN c.provider_type = 'capi' THEN jsonb_build_object(
							'infra_machine_template', np.capi_infra_machine_template,
							'bootstrap_files', np.capi_bootstrap_files,
							'pre_kubeadm_commands', to_jsonb(np.capi_pre_kubeadm_commands),
							'spire_attestation_method', np.capi_spire_attestation_method
						)
						ELSE NULL
					END
				FROM clusters c
				WHERE c.id = np.cluster_id
			`); err != nil {
				return fmt.Errorf("failed to backfill node pool provider payloads: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE node_sets ns
				SET
					nico = CASE
						WHEN c.provider_type = 'nico' THEN jsonb_build_object(
							'subnet_ids', to_jsonb(ns.subnet_ids),
							'vpc_prefix_ids', to_jsonb(ns.vpc_prefix_ids),
							'os_profile', ns.os_profile,
							'config_profile', ns.config_profile,
							'disk_selection_strategy', ns.disk_selection_strategy
						)
						ELSE NULL
					END,
					capi = CASE
						WHEN c.provider_type = 'capi' THEN jsonb_build_object(
							'infra_machine_template', ns.capi_infra_machine_template,
							'bootstrap_files', ns.capi_bootstrap_files,
							'pre_kubeadm_commands', to_jsonb(ns.capi_pre_kubeadm_commands),
							'spire_attestation_method', ns.capi_spire_attestation_method
						)
						ELSE NULL
					END
				FROM node_pools np
				JOIN clusters c ON c.id = np.cluster_id
				WHERE np.id = ns.node_pool_id
			`); err != nil {
				return fmt.Errorf("failed to backfill node set provider payloads: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE nodes
				SET nico = jsonb_build_object(
					'forge_instance_id', forge_instance_id,
					'forge_machine_id', forge_machine_id,
					'instance_type_id', instance_type_id
				)
			`); err != nil {
				return fmt.Errorf("failed to backfill node provider payloads: %w", err)
			}
			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
