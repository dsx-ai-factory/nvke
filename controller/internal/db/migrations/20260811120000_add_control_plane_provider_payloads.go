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

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.ExecContext(ctx, `
			UPDATE control_plane_configurations cpc
			SET config = CASE
				WHEN c.provider_type = 'nico' THEN
					jsonb_set(
						cpc.config - 'capi',
						'{nico}',
						jsonb_build_object(
							'instance_type_preference', cpc.config->'instance_type_preference',
							'subnet_ids', cpc.config->'subnet_ids',
							'vpc_prefix_ids', cpc.config->'vpc_prefix_ids',
							'os_profile', cpc.config->>'os_profile',
							'disk_selection_strategy', cpc.config->>'disk_selection_strategy',
							'config_profile', cpc.config->>'config_profile',
							'ssh_key_group_ids', cpc.config->'ssh_key_group_ids'
						),
						true
					)
				WHEN c.provider_type = 'capi' THEN
					jsonb_set(
						cpc.config - 'nico',
						'{capi}',
						jsonb_build_object(
							'infra_machine_template_preferences', cpc.config->'infra_machine_template_preferences',
							'spire_attestation_method', cpc.config->>'capi_spire_attestation_method',
							'bootstrap_files', cpc.config->'capi_bootstrap_files',
							'pre_kubeadm_commands', cpc.config->'capi_pre_kubeadm_commands'
						),
						true
					)
				ELSE cpc.config
			END
			FROM clusters c
			WHERE c.id = cpc.cluster_id
		`); err != nil {
			return fmt.Errorf("failed to backfill control plane provider payloads: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
