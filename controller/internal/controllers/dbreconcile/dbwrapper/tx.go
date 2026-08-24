// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/db"
)

type reconcileTx struct {
	db.Tx
	clusters  []db.Cluster
	nodePools []db.NodePool
	nodeSets  []db.NodeSet
	nodes     []db.Node
}

func (tx *reconcileTx) CreateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	created, err := tx.Tx.CreateCluster(ctx, cluster)
	if err != nil {
		return created, err
	}
	tx.clusters = append(tx.clusters, created)
	return created, nil
}

func (tx *reconcileTx) UpdateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	previous, err := tx.Tx.GetCluster(ctx, cluster.ID)
	if err != nil {
		return db.Cluster{}, err
	}

	updated, err := tx.Tx.UpdateCluster(ctx, cluster)
	if err != nil {
		return updated, err
	}
	tx.clusters = append(tx.clusters, previous, updated)
	return updated, nil
}

func (tx *reconcileTx) DeleteCluster(ctx context.Context, id uuid.UUID) error {
	if err := tx.Tx.DeleteCluster(ctx, id); err != nil {
		return err
	}
	tx.clusters = append(tx.clusters, db.Cluster{ID: id})
	return nil
}

func (tx *reconcileTx) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	created, err := tx.Tx.CreateNodePool(ctx, nodePool)
	if err != nil {
		return created, err
	}
	tx.nodePools = append(tx.nodePools, created)
	return created, nil
}

func (tx *reconcileTx) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	previous, err := tx.Tx.GetNodePool(ctx, nodePool.ID)
	if err != nil {
		return db.NodePool{}, err
	}

	updated, err := tx.Tx.UpdateNodePool(ctx, nodePool)
	if err != nil {
		return updated, err
	}
	tx.nodePools = append(tx.nodePools, previous, updated)
	return updated, nil
}

func (tx *reconcileTx) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	deleted, err := tx.Tx.GetNodePool(ctx, nodePoolID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			if err := tx.Tx.DeleteNodePool(ctx, nodePoolID); err != nil {
				return err
			}
			tx.nodePools = append(tx.nodePools, db.NodePool{ID: nodePoolID})
			return nil
		}
		return err
	}

	if err := tx.Tx.DeleteNodePool(ctx, nodePoolID); err != nil {
		return err
	}
	tx.nodePools = append(tx.nodePools, deleted)
	return nil
}

func (tx *reconcileTx) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	created, err := tx.Tx.CreateNodeSet(ctx, nodeSet)
	if err != nil {
		return created, err
	}
	tx.nodeSets = append(tx.nodeSets, created)
	return created, nil
}

func (tx *reconcileTx) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	previous, err := tx.Tx.GetNodeSet(ctx, nodeSet.ID)
	if err != nil {
		return db.NodeSet{}, err
	}

	updated, err := tx.Tx.UpdateNodeSet(ctx, nodeSet)
	if err != nil {
		return updated, err
	}
	tx.nodeSets = append(tx.nodeSets, previous, updated)
	return updated, nil
}

func (tx *reconcileTx) DeleteNodeSet(ctx context.Context, nodeSetID uuid.UUID) error {
	deleted, err := tx.Tx.GetNodeSet(ctx, nodeSetID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			if err := tx.Tx.DeleteNodeSet(ctx, nodeSetID); err != nil {
				return err
			}
			tx.nodeSets = append(tx.nodeSets, db.NodeSet{ID: nodeSetID})
			return nil
		}
		return err
	}

	if err := tx.Tx.DeleteNodeSet(ctx, nodeSetID); err != nil {
		return err
	}
	tx.nodeSets = append(tx.nodeSets, deleted)
	return nil
}

func (tx *reconcileTx) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	created, err := tx.Tx.CreateNode(ctx, node)
	if err != nil {
		return created, err
	}
	tx.nodes = append(tx.nodes, created)
	return created, nil
}

func (tx *reconcileTx) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	previous, err := tx.Tx.GetNode(ctx, node.ID)
	if err != nil {
		return db.Node{}, err
	}

	updated, err := tx.Tx.UpdateNode(ctx, node)
	if err != nil {
		return updated, err
	}
	tx.nodes = append(tx.nodes, previous, updated)
	return updated, nil
}

func (tx *reconcileTx) UpdateNodeForgeDetails(ctx context.Context, nodeID uuid.UUID, forgeInstanceID, forgeMachineID, ip *string) error {
	if forgeInstanceID == nil && forgeMachineID == nil && ip == nil {
		return tx.Tx.UpdateNodeForgeDetails(ctx, nodeID, forgeInstanceID, forgeMachineID, ip)
	}

	previous, err := tx.Tx.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}

	if err := tx.Tx.UpdateNodeForgeDetails(ctx, nodeID, forgeInstanceID, forgeMachineID, ip); err != nil {
		return err
	}

	updated, err := tx.Tx.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}

	tx.nodes = append(tx.nodes, previous, updated)
	return nil
}

func (tx *reconcileTx) DeleteNode(ctx context.Context, nodeID uuid.UUID) error {
	node, err := tx.Tx.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return tx.Tx.DeleteNode(ctx, nodeID)
		}
		return err
	}

	if err := tx.Tx.DeleteNode(ctx, nodeID); err != nil {
		return err
	}
	tx.nodes = append(tx.nodes, node)
	return nil
}
