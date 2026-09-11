// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// Makes the flat provider columns nullable, ahead of dropping them.
//
// This release stops writing them: the nico and capi payloads are the source of
// truth, and the fields are gone from the models, so Bun no longer names these
// columns in an INSERT. They are still NOT NULL, which would reject every one
// of those inserts, hence this.
//
// Dropping the columns has to wait for a later release. During a rolling
// upgrade the previous binary is still writing them, and it fails with 42703
// the moment they go.
func init() { //nolint:gochecknoinits // this is the typical way to register bun migrations
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE clusters
					ALTER COLUMN org_id DROP NOT NULL,
					ALTER COLUMN site_id DROP NOT NULL,
					ALTER COLUMN vpc_id DROP NOT NULL,
					ALTER COLUMN capi_infra_cluster DROP NOT NULL
			`); err != nil {
				return fmt.Errorf("failed to relax legacy clusters columns: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE node_pools
					ALTER COLUMN subnet_ids DROP NOT NULL,
					ALTER COLUMN vpc_prefix_ids DROP NOT NULL,
					ALTER COLUMN instance_type_id DROP NOT NULL,
					ALTER COLUMN infiniband_partition_id DROP NOT NULL,
					ALTER COLUMN nv_link_logical_partition_id DROP NOT NULL,
					ALTER COLUMN ssh_key_group_i_ds DROP NOT NULL,
					ALTER COLUMN os_profile DROP NOT NULL,
					ALTER COLUMN config_profile DROP NOT NULL,
					ALTER COLUMN disk_selection_strategy DROP NOT NULL,
					ALTER COLUMN capi_infra_machine_template DROP NOT NULL,
					ALTER COLUMN capi_bootstrap_files DROP NOT NULL,
					ALTER COLUMN capi_pre_kubeadm_commands DROP NOT NULL,
					ALTER COLUMN capi_spire_attestation_method DROP NOT NULL
			`); err != nil {
				return fmt.Errorf("failed to relax legacy node_pools columns: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE node_sets
					ALTER COLUMN subnet_ids DROP NOT NULL,
					ALTER COLUMN vpc_prefix_ids DROP NOT NULL,
					ALTER COLUMN os_profile DROP NOT NULL,
					ALTER COLUMN config_profile DROP NOT NULL,
					ALTER COLUMN disk_selection_strategy DROP NOT NULL,
					ALTER COLUMN capi_infra_machine_template DROP NOT NULL,
					ALTER COLUMN capi_bootstrap_files DROP NOT NULL,
					ALTER COLUMN capi_pre_kubeadm_commands DROP NOT NULL,
					ALTER COLUMN capi_spire_attestation_method DROP NOT NULL
			`); err != nil {
				return fmt.Errorf("failed to relax legacy node_sets columns: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE nodes
					ALTER COLUMN instance_type_id DROP NOT NULL,
					ALTER COLUMN forge_instance_id DROP NOT NULL,
					ALTER COLUMN forge_machine_id DROP NOT NULL
			`); err != nil {
				return fmt.Errorf("failed to relax legacy nodes columns: %w", err)
			}
			return nil
		})
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
