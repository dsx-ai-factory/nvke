// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// MigrateControlPlaneToController re-parents legacy node-pool-managed
// control-plane nodes onto the control-plane controller for a site that has
// opted in via PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER.
//
// This is a temporary transition helper: once every site has been migrated and
// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER is on by default, there
// are no node-pool-managed control planes left to convert and this function
// (and its call site in the worker) should be removed.
//
// It is a boot-time, flag-gated, idempotent sweep intended to run once per
// process start, after schema migrations and before the controllers and the
// Temporal worker start. Running it before the worker starts means no activity
// is mutating nodes while ownership is rewritten.
//
// Behavior:
//
//   - When useController is false it is a no-op. Unlike a bun migration nothing
//     is recorded, so a later flag flip (which requires a restart, since the flag
//     is an environment variable) runs the sweep on the next boot.
//   - Once the flag is on, cluster create rejects control-plane node pools
//     (see internal/server/clusters.go), so the set of clusters needing migration
//     is fixed at flip time and only shrinks; a single sweep per boot suffices.
//   - Re-parenting is pure metadata: each pooled control-plane node has its
//     node_pool_id / node_set_id cleared and is pinned to the cluster's existing
//     control-plane configuration snapshot (clusters.configuration_id). The
//     emptied control-plane node pools and their node sets are then deleted.
//   - Deleting clusters are skipped; their teardown removes the pools anyway.
//   - It fails closed: if any non-deleting cluster cannot be fully migrated (e.g.
//     it is missing a configuration snapshot) it returns an error, so the caller
//     can refuse to start until the issue is resolved rather than serve a
//     half-migrated fleet. The whole sweep runs in one transaction, so a failure
//     leaves the database unchanged and the next boot retries.
func MigrateControlPlaneToController(ctx context.Context, bunDB *bun.DB, useController bool) error {
	if !useController {
		return nil
	}

	tx, err := bunDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin control-plane migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback is a no-op after a successful Commit; tx errors are handled at Commit.

	// 1. Re-parent every pooled control-plane node to controller management,
	//    pinning the cluster's existing control-plane configuration snapshot.
	//    Clusters without a snapshot, or that are being deleted, are left alone.
	if _, err := tx.NewRaw(`
		UPDATE nodes AS n
		SET node_pool_id = NULL,
		    node_set_id = NULL,
		    configuration_id = c.configuration_id,
		    updated_at = current_timestamp
		FROM clusters AS c
		WHERE n.cluster_id = c.id
		  AND n.control_plane = TRUE
		  AND n.node_pool_id IS NOT NULL
		  AND c.configuration_id IS NOT NULL
		  AND COALESCE(c.conditions->'deleting'->>'status', '') <> 'True'
	`).Exec(ctx); err != nil {
		return fmt.Errorf("re-parent control-plane nodes: %w", err)
	}

	// 2. Detach the current node set pointer from now-empty control-plane pools
	//    so their node sets can be deleted without violating the FK.
	if _, err := tx.NewRaw(`
		UPDATE node_pools AS p
		SET current_node_set_id = NULL
		WHERE p.control_plane = TRUE
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_pool_id = p.id)
	`).Exec(ctx); err != nil {
		return fmt.Errorf("clear current node set on emptied control-plane pools: %w", err)
	}

	// 3. Delete the node sets belonging to the now-empty control-plane pools.
	if _, err := tx.NewRaw(`
		DELETE FROM node_sets AS ns
		USING node_pools AS p
		WHERE ns.node_pool_id = p.id
		  AND p.control_plane = TRUE
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_pool_id = p.id)
	`).Exec(ctx); err != nil {
		return fmt.Errorf("delete node sets of emptied control-plane pools: %w", err)
	}

	// 4. Delete the now-empty control-plane node pools. Pools whose cluster was
	//    skipped in step 1 still have nodes referencing them and survive.
	if _, err := tx.NewRaw(`
		DELETE FROM node_pools AS p
		WHERE p.control_plane = TRUE
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_pool_id = p.id)
	`).Exec(ctx); err != nil {
		return fmt.Errorf("delete emptied control-plane node pools: %w", err)
	}

	// 5. Safety net: a non-deleting cluster that still has pooled control-plane
	//    nodes could not be migrated (missing configuration snapshot). Fail loudly
	//    so the caller refuses to start and the sweep is retried on the next boot,
	//    rather than leaving a half-migrated fleet.
	var leftover int
	if err := tx.NewRaw(`
		SELECT count(*)
		FROM nodes n
		JOIN clusters c ON c.id = n.cluster_id
		WHERE n.control_plane = TRUE
		  AND n.node_pool_id IS NOT NULL
		  AND COALESCE(c.conditions->'deleting'->>'status', '') <> 'True'
	`).Scan(ctx, &leftover); err != nil {
		return fmt.Errorf("verify control-plane migration completeness: %w", err)
	}
	if leftover > 0 {
		return fmt.Errorf("control-plane migration incomplete: %d pooled control-plane node(s) remain; cluster missing configuration_id?", leftover)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit control-plane migration: %w", err)
	}

	return nil
}
