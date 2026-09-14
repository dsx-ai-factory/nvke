// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoinits // this is the typical way to register bun migrations
package migrations

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewUpdate().
			Table("clusters").
			Set(
				`capi_infra_cluster = jsonb_build_object(
					'apiVersion', ?,
					'kind', ?,
					'spec', jsonb_build_object('siteID', site_id::text)
				)`,
				nicov1.GroupVersion.String(),
				"NicoCluster",
			).
			Where("site_id != ?", uuid.Nil).
			Where("(capi_infra_cluster = '{}'::jsonb OR capi_infra_cluster = 'null'::jsonb)").
			Where("conditions->'deleting'->>'status' IS DISTINCT FROM 'True'").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to backfill Forge CAPI infra cluster manifests: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
