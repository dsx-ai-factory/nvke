// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// Retype node_pools.node_labels from text[] to a jsonb object defaulting to {}.
// Add node_pools.node_taints as a jsonb array defaulting to [].
// Control-plane configurations that store node_labels as an array are rewritten to {}.
//
// No backfill of the old node_labels values. The API stored them, and kubeadm
// never rendered them, so they were not applied to Kubernetes nodes. Labels
// already on a node stay. The existing fleet nodeLabels value is expected to be empty. Even if a
// row has values, they may not have been meant as node labels, so omitting them
// is safer given it doesn't impact labels already on a k8s node.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// Commit these statements in a single transaction. new release reads node_labels as a
		// jsonb object and selects node_taints. previous code reads
		// node_labels as text[]. a partial apply would leave a schema that neither release can read.
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			statements := []string{
				// 1. Retype node_pools.node_labels from text[] to a jsonb object defaulting to {}.
				// NOTE: USING '{}' replaces every existing value. Do not rerun this statement,
				// and do not copy it into a squashed migration: a later run would clear
				// jsonb maps stored after this migration.
				`ALTER TABLE node_pools
					ALTER COLUMN node_labels TYPE jsonb USING '{}'::jsonb`,
				`ALTER TABLE node_pools ALTER COLUMN node_labels SET DEFAULT '{}'::jsonb`,
				// 2. Add new column node_pools.node_taints as a jsonb array defaulting to [].
				`ALTER TABLE node_pools ADD COLUMN node_taints jsonb NOT NULL DEFAULT '[]'::jsonb`,
				// 3. Control-plane configurations that store node_labels as an array are rewritten to {}.
				// new code decodes snapshot node_labels into a map, so a JSON array would fail to load without the rewrite.
				// The WHERE clause skips a snapshot that has no node_labels key, given the new code reads missing key as no labels.
				`UPDATE control_plane_configurations
					SET config = jsonb_set(config, '{node_labels}', '{}'::jsonb)
					WHERE jsonb_typeof(config->'node_labels') = 'array'`,
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("node pool labels and taints: %w", err)
				}
			}
			return nil
		})
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
