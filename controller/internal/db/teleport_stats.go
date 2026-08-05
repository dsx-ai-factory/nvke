// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db

import (
	"context"

	"github.com/google/uuid"
)

// TeleportRolloutRow is one row per node pool, with cluster and node-level aggregates
// for the Teleport rollout metrics collector.
type TeleportRolloutRow struct {
	SiteID          uuid.UUID `bun:"site_id"`
	ClusterID       uuid.UUID `bun:"cluster_id"`
	ClusterName     string    `bun:"cluster_name"`
	ClusterEnabled  bool      `bun:"cluster_teleport_enabled"`
	NodePoolID      uuid.UUID `bun:"node_pool_id"`
	NodePoolName    string    `bun:"node_pool_name"`
	NodePoolEnabled bool      `bun:"node_pool_teleport_enabled"`
	NodesEligible   int       `bun:"nodes_eligible"`
	NodesRegistered int       `bun:"nodes_registered"`
}

// ListTeleportRolloutStats returns per-node-pool aggregates for Teleport metrics.
// Returns all clusters and node pools so the collector can emit both enabled=1 and enabled=0 gauges.
// Node counts only include nodes in teleport-enabled node sets.
func (q *queriesImpl) ListTeleportRolloutStats(ctx context.Context) ([]TeleportRolloutRow, error) {
	var rows []TeleportRolloutRow
	err := q.db.NewRaw(`
		SELECT
			c.site_id,
			c.id as cluster_id, c.name as cluster_name, c.teleport_enabled as cluster_teleport_enabled,
			np.id as node_pool_id, np.name as node_pool_name,
			(COALESCE(MAX(CASE WHEN ns.teleport_enabled = true THEN 1 ELSE 0 END), 0) > 0) as node_pool_teleport_enabled,
			COALESCE(SUM(CASE WHEN ns.teleport_enabled = true
				AND n.tpm_ek_pub_hash != ''
				AND COALESCE(n.conditions->'deleting'->>'status', 'False') != 'True'
				THEN 1 ELSE 0 END), 0) as nodes_eligible,
			COALESCE(SUM(CASE WHEN ns.teleport_enabled = true
				AND n.tpm_ek_pub_hash != ''
				AND COALESCE(n.conditions->'deleting'->>'status', 'False') != 'True'
				AND COALESCE(n.conditions->'teleportRegistered'->>'status', 'False') = 'True'
				THEN 1 ELSE 0 END), 0) as nodes_registered
		FROM clusters c
		JOIN node_pools np ON np.cluster_id = c.id
		LEFT JOIN node_sets ns ON ns.node_pool_id = np.id
		LEFT JOIN nodes n ON n.node_set_id = ns.id
		GROUP BY c.site_id, c.id, c.name, c.teleport_enabled, np.id, np.name
		ORDER BY c.site_id, c.name, np.name
	`).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
