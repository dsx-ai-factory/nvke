// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// Drops the flat provider columns, the contract half of the migration to nested
// nico/capi payloads and the last of three releases:
//
//  1. Stop reading and deriving from them, while still writing them (removed
//     SyncProviderFields, which rebuilt each payload *from* these columns).
//  2. Stop writing them, and drop their NOT NULL constraints
//     (20260909120000_relax_legacy_provider_columns) so the inserts still work.
//  3. This migration.
//
// The ordering is what makes it safe: a binary that still derives payloads from
// these columns rebuilds the payload on every write, including read-modify-write
// paths such as UpdateClusterConditions that never call Set*, so it would rebuild
// out of columns that no longer exist. Steps 1 and 2 are each reversible; this
// one is not, so it must not ship until step 2 is released.
//
// clusters.provider_type stays: it is the discriminator, not a provider field.
func init() { //nolint:gochecknoinits // this is the typical way to register bun migrations
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE clusters
					DROP COLUMN IF EXISTS org_id,
					DROP COLUMN IF EXISTS site_id,
					DROP COLUMN IF EXISTS vpc_id,
					DROP COLUMN IF EXISTS capi_infra_cluster
			`); err != nil {
				return fmt.Errorf("drop legacy clusters columns: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE node_pools
					DROP COLUMN IF EXISTS subnet_ids,
					DROP COLUMN IF EXISTS vpc_prefix_ids,
					DROP COLUMN IF EXISTS instance_type_id,
					DROP COLUMN IF EXISTS infiniband_partition_id,
					DROP COLUMN IF EXISTS nv_link_logical_partition_id,
					DROP COLUMN IF EXISTS ssh_key_group_i_ds,
					DROP COLUMN IF EXISTS os_profile,
					DROP COLUMN IF EXISTS config_profile,
					DROP COLUMN IF EXISTS disk_selection_strategy,
					DROP COLUMN IF EXISTS capi_infra_machine_template,
					DROP COLUMN IF EXISTS capi_bootstrap_files,
					DROP COLUMN IF EXISTS capi_pre_kubeadm_commands,
					DROP COLUMN IF EXISTS capi_spire_attestation_method
			`); err != nil {
				return fmt.Errorf("drop legacy node_pools columns: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE node_sets
					DROP COLUMN IF EXISTS subnet_ids,
					DROP COLUMN IF EXISTS vpc_prefix_ids,
					DROP COLUMN IF EXISTS os_profile,
					DROP COLUMN IF EXISTS config_profile,
					DROP COLUMN IF EXISTS disk_selection_strategy,
					DROP COLUMN IF EXISTS capi_infra_machine_template,
					DROP COLUMN IF EXISTS capi_bootstrap_files,
					DROP COLUMN IF EXISTS capi_pre_kubeadm_commands,
					DROP COLUMN IF EXISTS capi_spire_attestation_method
			`); err != nil {
				return fmt.Errorf("drop legacy node_sets columns: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE nodes
					DROP COLUMN IF EXISTS instance_type_id,
					DROP COLUMN IF EXISTS forge_instance_id,
					DROP COLUMN IF EXISTS forge_machine_id
			`); err != nil {
				return fmt.Errorf("drop legacy nodes columns: %w", err)
			}
			return nil
		})
	}, func(_ context.Context, _ *bun.DB) error {
		// Intentionally irreversible: the payloads are the source of truth and we
		// do not recreate dead columns on a downgrade. Fail rather than return nil,
		// which bun would treat as a successful rollback and mark the migration
		// unapplied while the columns stay dropped.
		return fmt.Errorf("down migration not supported")
	})
}
