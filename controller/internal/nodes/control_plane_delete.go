// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

var (
	// ErrClusterNotFound is returned when a live delete cannot find the cluster.
	ErrClusterNotFound = errors.New("cluster not found")
	// ErrClusterDeleting is returned when cluster teardown already owns the
	// cluster; callers should treat the request as a no-op success.
	ErrClusterDeleting = errors.New("cluster is deleting")
	// ErrLastJoinedControlPlaneNode is returned when a live node delete would
	// leave no joined control-plane node.
	ErrLastJoinedControlPlaneNode = errors.New("cannot delete the last joined control-plane node; delete the cluster instead")
	// ErrLastJoinedControlPlaneNodePool is returned when a live node-pool delete
	// would leave no joined control-plane node.
	ErrLastJoinedControlPlaneNodePool = errors.New("cannot delete the last joined control-plane node pool; delete the cluster instead")
	// ErrControlPlaneVIPRequired is returned when deleting a control-plane node
	// would make the controller replace it without a stable API endpoint.
	ErrControlPlaneVIPRequired = errors.New("controlPlaneVIP is required to replace a control-plane node; recreate the cluster with controlPlaneVIP configured")
	// ErrNodePoolDeleting is returned when node-pool teardown is already in
	// progress; callers should treat the request as a no-op success.
	ErrNodePoolDeleting = errors.New("node pool is deleting")
	// ErrNodeDeleting is returned when node teardown is already in progress;
	// callers should treat the request as a no-op success.
	ErrNodeDeleting = errors.New("node is deleting")
)

// guardLiveNodeDelete locks the cluster row first and then the node row for the
// remainder of the transaction, rejecting deletes that race teardown or would
// remove the last joined control plane. Cluster-first ordering matches the
// control-plane reconcile transaction (cluster then node), so the two cannot
// deadlock. Callers must finish marking the node deleting before committing so
// the cluster lock serializes concurrent live deletes.
func guardLiveNodeDelete(ctx context.Context, tx db.Tx, nodeID uuid.UUID) (db.Node, error) {
	// Read the node without a lock first, solely to learn its cluster so we can
	// take the cluster lock before the node lock (global cluster-first order).
	node, err := tx.GetNode(ctx, nodeID)
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to get node: %w", err)
	}

	// Cluster FOR UPDATE is the shared mutex for live deletes: it serializes
	// concurrent last-CP races (two txs each thinking the other survives) and
	// must stay held through the caller's mark-deleting/commit. It also rejects
	// when cluster teardown already owns the cluster (ErrClusterDeleting).
	cluster, err := lockClusterForLiveDelete(ctx, tx, node.ClusterID)
	if err != nil {
		return db.Node{}, err
	}

	// Re-read the node FOR UPDATE under the cluster lock for the authoritative
	// row; re-check Deleting on it in case it changed since the unlocked read.
	dbNode, err := tx.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to get node: %w", err)
	}

	if dbNode.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return db.Node{}, ErrNodeDeleting
	}

	// Check pool deleting under the cluster lock so a concurrent pool delete
	// that already committed is visible before we schedule node teardown.
	if dbNode.NodePoolID != uuid.Nil {
		nodePool, err := tx.GetNodePool(ctx, dbNode.NodePoolID)
		if err != nil {
			return db.Node{}, fmt.Errorf("failed to get node pool: %w", err)
		}
		if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return db.Node{}, ErrNodePoolDeleting
		}
	}

	if dbNode.ControlPlane {
		if strings.TrimSpace(cluster.ControlPlaneVIP) == "" {
			return db.Node{}, ErrControlPlaneVIPRequired
		}
		if err := ensureSurvivingJoinedControlPlane(ctx, tx, dbNode.ClusterID, liveControlPlaneDeleteExclusion{
			ExcludeNodeID: &dbNode.ID,
		}); err != nil {
			return db.Node{}, err
		}
	}

	return dbNode, nil
}

// guardLiveNodePoolDelete locks the node pool (and cluster) for the remainder
// of the transaction and rejects deletes that race teardown or would remove
// the last joined control plane. Callers must finish marking the pool deleting
// before committing so the cluster lock serializes concurrent live deletes.
func guardLiveNodePoolDelete(ctx context.Context, tx db.Tx, nodePoolID uuid.UUID) (db.NodePool, error) {
	// Read the pool without a lock first, solely to learn its cluster so we can
	// take the cluster lock before the pool lock (global cluster-first order).
	nodePool, err := tx.GetNodePool(ctx, nodePoolID)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to get node pool: %w", err)
	}

	// Same cluster-row mutex as guardLiveNodeDelete: serialize last-CP races and
	// reject deletes once cluster teardown is in progress. Lock is held until
	// this transaction commits after the caller marks the pool deleting.
	if _, err := lockClusterForLiveDelete(ctx, tx, nodePool.ClusterID); err != nil {
		return db.NodePool{}, err
	}

	// Re-read the pool FOR UPDATE under the cluster lock for the authoritative
	// row; re-check Deleting on it in case it changed since the unlocked read.
	dbNodePool, err := tx.GetNodePoolForUpdate(ctx, nodePoolID)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to get node pool: %w", err)
	}

	if dbNodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return db.NodePool{}, ErrNodePoolDeleting
	}

	if dbNodePool.ControlPlane {
		if err := ensureSurvivingJoinedControlPlane(ctx, tx, dbNodePool.ClusterID, liveControlPlaneDeleteExclusion{
			ExcludeNodePoolID: &dbNodePool.ID,
		}); err != nil {
			return db.NodePool{}, err
		}
	}

	return dbNodePool, nil
}

// liveControlPlaneDeleteExclusion describes which control-plane capacity a live
// DELETE is about to remove.
type liveControlPlaneDeleteExclusion struct {
	// ExcludeNodeID, when set, is the control-plane node being deleted.
	ExcludeNodeID *uuid.UUID
	// ExcludeNodePoolID, when set, is the control-plane node pool being deleted.
	// All nodes in that pool are treated as removed for the surviving-CP count.
	ExcludeNodePoolID *uuid.UUID
}

// lockClusterForLiveDelete locks the cluster row for the remainder of the
// transaction so concurrent live deletes serialize on a shared mutex. Returns
// ErrClusterDeleting when cluster teardown already owns the cluster.
func lockClusterForLiveDelete(ctx context.Context, tx db.Tx, clusterID uuid.UUID) (db.Cluster, error) {
	cluster, err := tx.GetClusterForUpdate(ctx, clusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Cluster{}, ErrClusterNotFound
		}
		return db.Cluster{}, fmt.Errorf("failed to get cluster: %w", err)
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return db.Cluster{}, ErrClusterDeleting
	}
	return cluster, nil
}

// ensureSurvivingJoinedControlPlane rejects a live control-plane node or
// node-pool delete that would leave no joined control-plane node. Removing the
// last control plane must go through cluster delete.
//
// Callers must hold the cluster row lock (via lockClusterForLiveDelete) for the
// rest of the transaction that marks the resource deleting, so two concurrent
// deletes of the only two control planes cannot both pass and then both succeed.
func ensureSurvivingJoinedControlPlane(
	ctx context.Context,
	tx db.Tx,
	clusterID uuid.UUID,
	exclusion liveControlPlaneDeleteExclusion,
) error {
	hasSurvivor, err := hasSurvivingJoinedControlPlane(ctx, tx, clusterID, exclusion)
	if err != nil {
		return err
	}
	if hasSurvivor {
		return nil
	}

	if exclusion.ExcludeNodePoolID != nil {
		return ErrLastJoinedControlPlaneNodePool
	}
	return ErrLastJoinedControlPlaneNode
}

func hasSurvivingJoinedControlPlane(
	ctx context.Context,
	tx db.Tx,
	clusterID uuid.UUID,
	exclusion liveControlPlaneDeleteExclusion,
) (bool, error) {
	count, err := countSurvivingJoinedControlPlane(ctx, tx, clusterID, exclusion)
	return count > 0, err
}

// countSurvivingJoinedControlPlane counts the joined control-plane nodes that
// would remain after the described delete. A node is not counted when it is the
// delete target, when it belongs to a node pool being deleted, when its own
// teardown is already in flight, or when a delete has been accepted for it but
// not started yet: all four are on their way out of the etcd membership.
func countSurvivingJoinedControlPlane(
	ctx context.Context,
	tx db.Tx,
	clusterID uuid.UUID,
	exclusion liveControlPlaneDeleteExclusion,
) (int, error) {
	controlPlane := true

	// Nodes don't carry their pool's Deleting bit, so we need the CP pools to
	// know which nodes are already (or about to be) removed with a pool delete.
	cpPools, err := tx.ListNodePools(ctx, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list control-plane node pools: %w", err)
	}

	cpPoolsMap := make(map[uuid.UUID]db.NodePool, len(cpPools))
	for _, pool := range cpPools {
		cpPoolsMap[pool.ID] = pool
	}

	cpNodes, err := tx.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list control-plane nodes: %w", err)
	}

	survivors := 0
	for _, node := range cpNodes {
		if exclusion.ExcludeNodeID != nil && node.ID == *exclusion.ExcludeNodeID {
			continue
		}
		if exclusion.ExcludeNodePoolID != nil && node.NodePoolID == *exclusion.ExcludeNodePoolID {
			continue
		}
		if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
			continue
		}
		// An accepted-but-not-yet-started delete removes this node too, so two
		// requests in a row cannot each count the other as a survivor.
		if node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue {
			continue
		}
		// node.NodePoolID may be uuid.Nil; that won't be in the map, so lookup is safe.
		nodePool := cpPoolsMap[node.NodePoolID]
		if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
			continue
		}
		if node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}
		survivors++
	}

	return survivors, nil
}
