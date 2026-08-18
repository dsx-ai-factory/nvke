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
	nodeIDs     []uuid.UUID
	nodePoolIDs []uuid.UUID
	nodeSetIDs  []uuid.UUID
}

func (tx *reconcileTx) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	created, err := tx.Tx.CreateNodePool(ctx, nodePool)
	if err != nil {
		return created, err
	}
	tx.nodePoolIDs = append(tx.nodePoolIDs, created.ID)
	return created, nil
}

func (tx *reconcileTx) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	updated, err := tx.Tx.UpdateNodePool(ctx, nodePool)
	if err != nil {
		return updated, err
	}
	tx.nodePoolIDs = append(tx.nodePoolIDs, updated.ID)
	return updated, nil
}

func (tx *reconcileTx) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	if err := tx.Tx.DeleteNodePool(ctx, nodePoolID); err != nil {
		return err
	}
	tx.nodePoolIDs = append(tx.nodePoolIDs, nodePoolID)
	return nil
}

func (tx *reconcileTx) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	created, err := tx.Tx.CreateNode(ctx, node)
	if err != nil {
		return created, err
	}
	tx.nodeIDs = append(tx.nodeIDs, created.ID)
	tx.appendNodeSetID(created.NodeSetID)
	return created, nil
}

func (tx *reconcileTx) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	updated, err := tx.Tx.UpdateNode(ctx, node)
	if err != nil {
		return updated, err
	}
	tx.nodeIDs = append(tx.nodeIDs, updated.ID)
	return updated, nil
}

func (tx *reconcileTx) DeleteNode(ctx context.Context, nodeID uuid.UUID) error {
	node, err := tx.Tx.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return tx.Tx.DeleteNode(ctx, nodeID)
		}
		return err
	}
	nodePoolID := node.NodePoolID
	if err := tx.Tx.DeleteNode(ctx, nodeID); err != nil {
		return err
	}
	if nodePoolID != uuid.Nil {
		tx.nodePoolIDs = append(tx.nodePoolIDs, nodePoolID)
	}
	tx.appendNodeSetID(node.NodeSetID)
	return nil
}

func (tx *reconcileTx) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	created, err := tx.Tx.CreateNodeSet(ctx, nodeSet)
	if err != nil {
		return created, err
	}
	tx.appendNodeSetID(created.ID)
	return created, nil
}

func (tx *reconcileTx) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	updated, err := tx.Tx.UpdateNodeSet(ctx, nodeSet)
	if err != nil {
		return updated, err
	}
	tx.appendNodeSetID(updated.ID)
	return updated, nil
}

func (tx *reconcileTx) appendNodeSetID(id uuid.UUID) {
	if id == uuid.Nil {
		return
	}
	tx.nodeSetIDs = append(tx.nodeSetIDs, id)
}
