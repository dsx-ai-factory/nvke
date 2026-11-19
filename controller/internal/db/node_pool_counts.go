// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// NodePoolCounts describes observed lifecycle records, including pending and
// deleting nodes. Updated nodes need not be provisioned or Kubernetes-ready.
type NodePoolCounts struct {
	NodeCount        int
	ReadyNodeCount   int
	UpdatedNodeCount int
}

type nodeSetCounts struct {
	NodePoolID     uuid.UUID
	NodeCount      int
	ReadyNodeCount int
	Snapshot       NodeSet `bun:"embed:snapshot_"`
}

// ListNodePoolCounts aggregates only the requested pools in one query. Snapshot
// configuration and node counts are read together; configuration matching uses
// the supplied pool versions (the same versions serialized by the API).
func (q *queriesImpl) ListNodePoolCounts(ctx context.Context, pools []NodePool) (map[uuid.UUID]NodePoolCounts, error) {
	counts := make(map[uuid.UUID]NodePoolCounts, len(pools))
	byID := make(map[uuid.UUID]NodePool, len(pools))
	ids := make([]uuid.UUID, 0, len(pools))
	for _, pool := range pools {
		counts[pool.ID] = NodePoolCounts{}
		byID[pool.ID] = pool
		ids = append(ids, pool.ID)
	}
	if len(ids) == 0 {
		return counts, nil
	}

	var groups []nodeSetCounts
	err := q.db.NewRaw(`
		SELECT nc.node_pool_id, nc.node_count, nc.ready_node_count,
		       ns.id AS snapshot_id, ns.node_pool_id AS snapshot_node_pool_id,
		       ns.kubernetes_version AS snapshot_kubernetes_version,
		       ns.system_reserved_memory AS snapshot_system_reserved_memory,
		       ns.kube_reserved_memory AS snapshot_kube_reserved_memory,
		       ns.teleport_enabled AS snapshot_teleport_enabled,
		       ns.nico AS snapshot_nico, ns.capi AS snapshot_capi,
		       ns.launch_layer AS snapshot_launch_layer
		FROM (
			SELECT node_pool_id, node_set_id, COUNT(*) AS node_count,
			       SUM(CASE WHEN conditions->'kubernetesReady'->>'status' = 'True'
			                THEN 1 ELSE 0 END) AS ready_node_count
			FROM nodes
			WHERE node_pool_id IN (?)
			GROUP BY node_pool_id, node_set_id
		) nc
		LEFT JOIN node_sets ns ON ns.id = nc.node_set_id
	`, bun.List(ids)).Scan(ctx, &groups)
	if err != nil {
		return nil, fmt.Errorf("aggregate node pool counts: %w", err)
	}

	for _, group := range groups {
		count := counts[group.NodePoolID]
		count.NodeCount += group.NodeCount
		count.ReadyNodeCount += group.ReadyNodeCount
		if group.Snapshot.ID != uuid.Nil && group.Snapshot.NodePoolID == group.NodePoolID &&
			NodePoolMatchesNodeSet(byID[group.NodePoolID], group.Snapshot) {
			count.UpdatedNodeCount += group.NodeCount
		}
		counts[group.NodePoolID] = count
	}
	return counts, nil
}
