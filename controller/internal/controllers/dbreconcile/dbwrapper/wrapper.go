// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package dbwrapper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	dbsource "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/source"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type Enqueuer interface {
	Enqueue(ctx context.Context, key dbreconcile.Key) error
}

type nodeSourceRegistration struct {
	filters  db.NodeFilters
	enqueuer Enqueuer
}

type nodePoolSourceRegistration struct {
	filters  db.NodePoolFilters
	enqueuer Enqueuer
}

type DB struct {
	db.DB

	logger          *slog.Logger
	lock            sync.RWMutex
	nodeSources     []nodeSourceRegistration
	nodePoolSources []nodePoolSourceRegistration
}

func New(database db.DB, logger *slog.Logger) *DB {
	if logger == nil {
		logger = slog.Default()
	}
	return &DB{
		DB:     database,
		logger: logger,
	}
}

func (w *DB) NodeSource(filters db.NodeFilters) *dbsource.Source[dbreconcile.Key] {
	nodeSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		nodeEventSource{
			queries: w,
			filters: filters,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodeSources = append(w.nodeSources, nodeSourceRegistration{
		filters:  filters,
		enqueuer: nodeSource,
	})
	return nodeSource
}

func (w *DB) NodePoolSource(filters db.NodePoolFilters) *dbsource.Source[dbreconcile.Key] {
	nodePoolSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		nodePoolEventSource{
			queries: w,
			filters: filters,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodePoolSources = append(w.nodePoolSources, nodePoolSourceRegistration{
		filters:  filters,
		enqueuer: nodePoolSource,
	})
	return nodePoolSource
}

func (w *DB) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	created, err := w.DB.CreateNodePool(ctx, nodePool)
	if err != nil {
		return created, err
	}
	if err := w.enqueueNodePool(ctx, created.ID); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	updated, err := w.DB.UpdateNodePool(ctx, nodePool)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueNodePool(ctx, updated.ID); err != nil {
		return updated, err
	}
	return updated, nil
}

func (w *DB) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	if err := w.DB.DeleteNodePool(ctx, nodePoolID); err != nil {
		return err
	}
	if err := w.enqueueNodePool(ctx, nodePoolID); err != nil {
		return err
	}
	return nil
}

func (w *DB) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	created, err := w.DB.CreateNode(ctx, node)
	if err != nil {
		return created, err
	}
	if err := w.enqueueNode(ctx, created.ID); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	updated, err := w.DB.UpdateNode(ctx, node)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueNode(ctx, updated.ID); err != nil {
		return updated, err
	}
	return updated, nil
}

func (w *DB) DeleteNode(ctx context.Context, nodeID uuid.UUID) error {
	nodePoolID, err := w.nodePoolIDForNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if err := w.DB.DeleteNode(ctx, nodeID); err != nil {
		return err
	}
	if nodePoolID == uuid.Nil {
		return nil
	}
	if err := w.enqueueNodePool(ctx, nodePoolID); err != nil {
		return err
	}
	return nil
}

func (w *DB) RunInTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error {
	tx := &reconcileTx{Tx: nil}
	err := w.DB.RunInTx(ctx, func(ctx context.Context, realTx db.Tx) error {
		tx.Tx = realTx
		return fn(ctx, tx)
	})
	if err != nil {
		return err
	}

	for _, nodeID := range tx.nodeIDs {
		if err := w.enqueueNode(ctx, nodeID); err != nil {
			w.logger.Warn("Failed to enqueue node after committed transaction", "nodeID", nodeID, "error", err)
		}
	}
	for _, nodePoolID := range tx.nodePoolIDs {
		if err := w.enqueueNodePool(ctx, nodePoolID); err != nil {
			w.logger.Warn("Failed to enqueue node pool after committed transaction", "nodePoolID", nodePoolID, "error", err)
		}
	}
	return nil
}

func (w *DB) enqueueNode(ctx context.Context, nodeID uuid.UUID) error {
	sources := func() []nodeSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		return append([]nodeSourceRegistration(nil), w.nodeSources...)
	}()

	for _, source := range sources {
		matches, err := w.nodeMatchesFilters(ctx, nodeID, source.filters)
		if err != nil {
			return err
		}
		if !matches {
			continue
		}
		if err := source.enqueuer.Enqueue(ctx, dbreconcile.NodeKey(nodeID)); err != nil {
			return fmt.Errorf("failed to enqueue node %s: %w", nodeID, err)
		}
	}
	return nil
}

func (w *DB) enqueueNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	sources := func() []nodePoolSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		return append([]nodePoolSourceRegistration(nil), w.nodePoolSources...)
	}()

	for _, source := range sources {
		if err := source.enqueuer.Enqueue(ctx, dbreconcile.NodePoolKey(nodePoolID)); err != nil {
			return fmt.Errorf("failed to enqueue node pool %s: %w", nodePoolID, err)
		}
	}
	return nil
}

func (w *DB) nodeMatchesFilters(ctx context.Context, nodeID uuid.UUID, filters db.NodeFilters) (bool, error) {
	filters.ID = &nodeID
	nodes, err := w.DB.ListNodes(ctx, filters)
	if err != nil {
		return false, fmt.Errorf("failed to list nodes for DB reconcile source filters: %w", err)
	}
	return len(nodes) > 0, nil
}

func (w *DB) nodePoolIDForNode(ctx context.Context, nodeID uuid.UUID) (uuid.UUID, error) {
	node, err := w.DB.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return uuid.Nil, nil
		}
		return uuid.Nil, fmt.Errorf("failed to get node %s before DB reconcile delete: %w", nodeID, err)
	}
	return node.NodePoolID, nil
}
