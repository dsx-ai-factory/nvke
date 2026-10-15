// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// pooledControlPlaneCluster summarizes a non-deleting cluster's pooled
// control-plane nodes after the migration's re-parenting steps.
type pooledControlPlaneCluster struct {
	ClusterID         uuid.UUID `bun:"cluster_id"`
	ClusterName       string    `bun:"cluster_name"`
	Nodes             int       `bun:"nodes"`
	HasSnapshotSource bool      `bun:"has_snapshot_source"`
}

// deprecatedControlPlaneNodeSet is a control-plane node set that is no longer
// its pool's current node set but still owns pooled control-plane nodes.
type deprecatedControlPlaneNodeSet struct {
	NodeSetID         uuid.UUID `bun:"node_set_id"`
	ClusterID         uuid.UUID `bun:"cluster_id"`
	CurrentConfigID   uuid.UUID `bun:"current_config_id"`
	Release           string    `bun:"release"`
	ConfigProfile     string    `bun:"config_profile"`
	KubernetesVersion string    `bun:"kubernetes_version"`
}

// SyncMissingControlPlaneConfigsFromCPNodePools creates a control-plane
// configuration snapshot for every non-deleting cluster that still has an
// active CP node pool but a nil clusters.configuration_id.
//
// MigrateControlPlaneToController calls this first. The helper is exported so
// the imported-site case can be unit-tested without running the Postgres-only
// re-parent SQL.
func SyncMissingControlPlaneConfigsFromCPNodePools(ctx context.Context, bunDB *bun.DB) error {
	var clusterIDs []uuid.UUID
	err := bunDB.NewSelect().
		TableExpr("clusters AS c").
		Column("c.id").
		Join("JOIN node_pools AS np ON np.cluster_id = c.id").
		Where("np.control_plane = ?", true).
		Where("c.configuration_id IS NULL").
		Distinct().
		Scan(ctx, &clusterIDs)
	if err != nil {
		return fmt.Errorf("list clusters missing control-plane configuration: %w", err)
	}
	if len(clusterIDs) == 0 {
		return nil
	}

	q := NewDB(bunDB)
	cp := true
	for _, id := range clusterIDs {
		cluster, err := q.GetCluster(ctx, id)
		if err != nil {
			return fmt.Errorf("load cluster %s: %w", id, err)
		}
		if cluster.Conditions.Deleting.Status == ConditionStatusTrue {
			continue
		}
		pools, err := q.ListNodePools(ctx, NodePoolFilters{ClusterID: &id, ControlPlane: &cp})
		if err != nil {
			return fmt.Errorf("list control-plane node pools for cluster %s: %w", id, err)
		}
		if !hasActiveControlPlaneNodePool(pools) {
			continue
		}
		if err := q.SyncClusterControlPlaneConfigFromCPNodePools(ctx, id); err != nil {
			return fmt.Errorf("sync control-plane config for cluster %s (%s): %w", cluster.Name, id, err)
		}
	}
	return nil
}

func hasActiveControlPlaneNodePool(pools []NodePool) bool {
	for i := range pools {
		if pools[i].ControlPlane && pools[i].Conditions.Deleting.Status != ConditionStatusTrue {
			return true
		}
	}
	return false
}

// MigrateControlPlaneToController enrolls every non-deleting cluster in the
// control-plane controller by re-parenting its pooled control-plane nodes onto
// the cluster's control-plane configuration snapshot, then deleting the emptied
// node pools and node sets.
//
// The sweep is idempotent and runs in one transaction on every worker boot,
// after schema migrations and before the controllers start. It returns an error
// if a non-deleting cluster with a control-plane snapshot source cannot be fully
// migrated; the worker then refuses to start. Temporary: remove it and its worker call site once every site is migrated.
func MigrateControlPlaneToController(ctx context.Context, bunDB *bun.DB, logger *slog.Logger) error {
	if err := SyncMissingControlPlaneConfigsFromCPNodePools(ctx, bunDB); err != nil {
		return err
	}

	tx, err := bunDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin control-plane migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback is a no-op after a successful Commit; tx errors are handled at Commit.

	// 1. Enumerate deprecated control-plane node sets (not their pool's current
	//    set but still owning pooled CP nodes) so each keeps a distinct snapshot.
	var deprecated []deprecatedControlPlaneNodeSet
	if err := tx.NewRaw(`
		SELECT ns.id                 AS node_set_id,
		       p.cluster_id          AS cluster_id,
		       c.configuration_id    AS current_config_id,
		       COALESCE(ns.nico->>'release', ns.nico->>'os_profile', '') AS release,
		       COALESCE(ns.nico->>'config_profile', '') AS config_profile,
		       ns.kubernetes_version AS kubernetes_version
		FROM node_sets ns
		JOIN node_pools p ON p.id = ns.node_pool_id
		JOIN clusters c ON c.id = p.cluster_id
		WHERE p.control_plane = TRUE
		  AND p.current_node_set_id IS NOT NULL
		  AND ns.id <> p.current_node_set_id
		  AND c.configuration_id IS NOT NULL
		  AND COALESCE(c.conditions->'deleting'->>'status', '') <> 'True'
		  AND EXISTS (
		      SELECT 1 FROM nodes n
		      WHERE n.node_set_id = ns.id
		        AND n.control_plane = TRUE
		        AND n.node_pool_id IS NOT NULL
		  )
	`).Scan(ctx, &deprecated); err != nil {
		return fmt.Errorf("list deprecated control-plane node sets: %w", err)
	}

	// For each deprecated generation, mint a snapshot (the cluster's current one
	// with the generation's rollout-relevant fields overlaid) and pin its nodes
	// to it. INSERT precedes UPDATE so the nodes.configuration_id FK is satisfied.
	for _, d := range deprecated {
		var newConfigID uuid.UUID
		if err := tx.NewRaw(`
			INSERT INTO control_plane_configurations (cluster_id, config, change_reason)
			SELECT ?,
			       cur.config
			         || jsonb_build_object('kubernetes_version', ?::text)
			         || CASE WHEN jsonb_typeof(cur.config->'nico') = 'object'
			                 THEN jsonb_build_object('nico',
			                        cur.config->'nico' || jsonb_build_object(
			                            'release', ?::text,
			                            'os_profile', ?::text,
			                            'config_profile', ?::text))
			                 ELSE '{}'::jsonb
			            END,
			       ?
			FROM control_plane_configurations cur
			WHERE cur.id = ?
			RETURNING id
		`, d.ClusterID, d.KubernetesVersion, d.Release, d.Release, d.ConfigProfile,
			"control-plane migration: preserve deprecated node generation for rollout",
			d.CurrentConfigID).Scan(ctx, &newConfigID); err != nil {
			return fmt.Errorf("snapshot deprecated control-plane node set %s: %w", d.NodeSetID, err)
		}

		if _, err := tx.NewRaw(`
			UPDATE nodes
			SET node_pool_id = NULL,
			    node_set_id = NULL,
			    configuration_id = ?,
			    updated_at = current_timestamp
			WHERE node_set_id = ?
			  AND control_plane = TRUE
			  AND node_pool_id IS NOT NULL
		`, newConfigID, d.NodeSetID).Exec(ctx); err != nil {
			return fmt.Errorf("re-parent deprecated control-plane nodes for node set %s: %w", d.NodeSetID, err)
		}
	}

	// 2. Re-parent the remaining pooled CP nodes (on their pool's current node
	//    set) to the cluster's current snapshot. Step 1's nodes are already
	//    detached; clusters without a snapshot or being deleted are left alone.
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

	// 3. Clear the current-node-set pointer on now-empty CP pools so their node
	//    sets can be deleted without violating the FK.
	if _, err := tx.NewRaw(`
		UPDATE node_pools AS p
		SET current_node_set_id = NULL
		WHERE p.control_plane = TRUE
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_pool_id = p.id)
	`).Exec(ctx); err != nil {
		return fmt.Errorf("clear current node set on emptied control-plane pools: %w", err)
	}

	// 4. Delete the node sets belonging to the now-empty control-plane pools.
	if _, err := tx.NewRaw(`
		DELETE FROM node_sets AS ns
		USING node_pools AS p
		WHERE ns.node_pool_id = p.id
		  AND p.control_plane = TRUE
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_pool_id = p.id)
	`).Exec(ctx); err != nil {
		return fmt.Errorf("delete node sets of emptied control-plane pools: %w", err)
	}

	// 5. Delete the now-empty CP node pools. Pools of skipped clusters still have
	//    nodes referencing them and survive.
	if _, err := tx.NewRaw(`
		DELETE FROM node_pools AS p
		WHERE p.control_plane = TRUE
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_pool_id = p.id)
	`).Exec(ctx); err != nil {
		return fmt.Errorf("delete emptied control-plane node pools: %w", err)
	}

	// 6. Safety net: any non-deleting cluster still holding pooled CP nodes could
	//    not be migrated (missing snapshot). Fail loudly so the worker refuses to
	//    start and retries next boot rather than serve a half-migrated fleet.
	//    A cluster with no snapshot and no active CP pool has nothing to snapshot
	//    from, so it is skipped with a warning.
	var pooled []pooledControlPlaneCluster
	if err := tx.NewRaw(`
		SELECT c.id    AS cluster_id,
		       c.name  AS cluster_name,
		       count(*) AS nodes,
		       (c.configuration_id IS NOT NULL OR EXISTS (
		           SELECT 1 FROM node_pools p
		           WHERE p.cluster_id = c.id
		             AND p.control_plane = TRUE
		             AND COALESCE(p.conditions->'deleting'->>'status', '') <> 'True'
		       )) AS has_snapshot_source
		FROM nodes n
		JOIN clusters c ON c.id = n.cluster_id
		WHERE n.control_plane = TRUE
		  AND n.node_pool_id IS NOT NULL
		  AND COALESCE(c.conditions->'deleting'->>'status', '') <> 'True'
		GROUP BY c.id, c.name, c.configuration_id
	`).Scan(ctx, &pooled); err != nil {
		return fmt.Errorf("verify control-plane migration completeness: %w", err)
	}
	leftover := 0
	for _, c := range pooled {
		if c.HasSnapshotSource {
			leftover += c.Nodes
			continue
		}
		logger.Warn("control-plane migration skipped cluster with no snapshot source",
			"clusterID", c.ClusterID,
			"clusterName", c.ClusterName,
			"pooledControlPlaneNodes", c.Nodes,
		)
	}
	if leftover > 0 {
		return fmt.Errorf("control-plane migration incomplete: %d pooled control-plane node(s) remain; cluster missing configuration_id?", leftover)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit control-plane migration: %w", err)
	}

	return nil
}
