// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// Moves os_profile to release in the nico payloads, leaving release as the only
// key naming a release.
//
// The osProfile-to-release rename kept writing both keys, because a CWE
// predating it rewrites the whole nico payload from its own struct and would
// erase a payload carrying only release. Reads fall back to os_profile, which is
// what still makes rows written before the rename readable: nothing has
// rewritten them, so they carry os_profile alone.
//
// This migration removes the need for that fallback, and must be applied before
// the fallback goes, or a row predating the rename reads as an empty release.
//
// Dropping os_profile is safe only once the rename has reached every
// environment, because a CWE predating it reads that key and nothing else. A CWE
// carrying the rename prefers release, so it reads a stripped row correctly and
// the drop is invisible to the pods still draining during the upgrade. Those
// pods write both keys, so a row one of them touches between this migration and
// the end of the rollout keeps an os_profile that nothing reads. The residue is
// inert and the next write through the new struct clears it.
//
// Idempotent: it matches only rows that still carry os_profile, fills release
// from it when release is missing, and leaves an existing release alone, so a
// re-run finds nothing to do. The jsonb_typeof guard skips the NULL and
// non-object payloads that CAPI-provider rows carry, and jsonb_exists is the
// function spelling of the ? operator, which bun would otherwise read as a
// placeholder.
func init() { //nolint:gochecknoinits // this is the typical way to register bun migrations
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				UPDATE node_pools
				SET nico = CASE
						WHEN COALESCE(nico->>'release', '') = '' AND COALESCE(nico->>'os_profile', '') <> ''
							THEN nico || jsonb_build_object('release', nico->>'os_profile')
						ELSE nico
					END - 'os_profile'
				WHERE jsonb_typeof(nico) = 'object'
				  AND jsonb_exists(nico, 'os_profile')
			`); err != nil {
				return fmt.Errorf("move node_pools.nico os_profile to release: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE node_sets
				SET nico = CASE
						WHEN COALESCE(nico->>'release', '') = '' AND COALESCE(nico->>'os_profile', '') <> ''
							THEN nico || jsonb_build_object('release', nico->>'os_profile')
						ELSE nico
					END - 'os_profile'
				WHERE jsonb_typeof(nico) = 'object'
				  AND jsonb_exists(nico, 'os_profile')
			`); err != nil {
				return fmt.Errorf("move node_sets.nico os_profile to release: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE control_plane_configurations
				SET config = jsonb_set(config, '{nico}', CASE
						WHEN COALESCE(config->'nico'->>'release', '') = '' AND COALESCE(config->'nico'->>'os_profile', '') <> ''
							THEN config->'nico' || jsonb_build_object('release', config->'nico'->>'os_profile')
						ELSE config->'nico'
					END - 'os_profile')
				WHERE jsonb_typeof(config->'nico') = 'object'
				  AND jsonb_exists(config->'nico', 'os_profile')
			`); err != nil {
				return fmt.Errorf("move control_plane_configurations.config nico os_profile to release: %w", err)
			}
			return nil
		})
	}, func(_ context.Context, _ *bun.DB) error {
		// Intentionally irreversible: release is the only key naming a release and
		// we do not write the mirror back on a downgrade. Fail rather than return
		// nil, which bun would treat as a successful rollback and mark the
		// migration unapplied while os_profile stays dropped, telling an operator
		// the database is ready for a CWE that reads os_profile when it is not.
		return fmt.Errorf("down migration not supported")
	})
}
