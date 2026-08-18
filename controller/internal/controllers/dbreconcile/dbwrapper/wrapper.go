// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

type nodeSetSourceRegistration struct {
	filters  db.NodeSetFilters
	enqueuer Enqueuer
}

type DB struct {
	db.DB

	logger          *slog.Logger
	lock            sync.RWMutex
	nodeSources     []nodeSourceRegistration
	nodePoolSources []nodePoolSourceRegistration
	nodeSetSources  []nodeSetSourceRegistration
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

func (w *DB) NodeSetSource(filters db.NodeSetFilters) *dbsource.Source[dbreconcile.Key] {
	nodeSetSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		nodeSetEventSource{
			queries: w,
			filters: filters,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodeSetSources = append(w.nodeSetSources, nodeSetSourceRegistration{
		filters:  filters,
		enqueuer: nodeSetSource,
	})
	return nodeSetSource
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
	if err := w.enqueueNodeSetForNode(ctx, created); err != nil {
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
	node, exists, err := w.nodeForDelete(ctx, nodeID)
	if err != nil {
		return err
	}
	if err := w.DB.DeleteNode(ctx, nodeID); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if node.NodePoolID != uuid.Nil {
		if err := w.enqueueNodePool(ctx, node.NodePoolID); err != nil {
			return err
		}
	}
	if err := w.enqueueNodeSetForNode(ctx, node); err != nil {
		return err
	}
	return nil
}

func (w *DB) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	created, err := w.DB.CreateNodeSet(ctx, nodeSet)
	if err != nil {
		return created, err
	}
	if err := w.enqueueNodeSet(ctx, created.ID); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	updated, err := w.DB.UpdateNodeSet(ctx, nodeSet)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueNodeSet(ctx, updated.ID); err != nil {
		return updated, err
	}
	return updated, nil
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
	for _, nodeSetID := range tx.nodeSetIDs {
		if err := w.enqueueNodeSet(ctx, nodeSetID); err != nil {
			w.logger.Warn("Failed to enqueue node set after committed transaction", "nodeSetID", nodeSetID, "error", err)
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

func (w *DB) enqueueNodeSet(ctx context.Context, nodeSetID uuid.UUID) error {
	if nodeSetID == uuid.Nil {
		return nil
	}

	sources := func() []nodeSetSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		return append([]nodeSetSourceRegistration(nil), w.nodeSetSources...)
	}()

	for _, source := range sources {
		matches, err := w.nodeSetMatchesFilters(ctx, nodeSetID, source.filters)
		if err != nil {
			return err
		}
		if !matches {
			continue
		}
		if err := source.enqueuer.Enqueue(ctx, dbreconcile.NodeSetKey(nodeSetID)); err != nil {
			return fmt.Errorf("failed to enqueue node set %s: %w", nodeSetID, err)
		}
	}
	return nil
}

func (w *DB) enqueueNodeSetForNode(ctx context.Context, node db.Node) error {
	return w.enqueueNodeSet(ctx, node.NodeSetID)
}

func (w *DB) nodeMatchesFilters(ctx context.Context, nodeID uuid.UUID, filters db.NodeFilters) (bool, error) {
	filters.ID = &nodeID
	nodes, err := w.DB.ListNodes(ctx, filters)
	if err != nil {
		return false, fmt.Errorf("failed to list nodes for DB reconcile source filters: %w", err)
	}
	return len(nodes) > 0, nil
}

func (w *DB) nodeSetMatchesFilters(ctx context.Context, nodeSetID uuid.UUID, filters db.NodeSetFilters) (bool, error) {
	nodeSetFilters := filters
	nodeSetFilters.NodeSetID = &nodeSetID
	nodeSets, err := w.DB.ListNodeSets(ctx, nodeSetFilters)
	if err != nil {
		return false, fmt.Errorf("failed to list node sets for DB reconcile source filters: %w", err)
	}
	return len(nodeSets) > 0, nil
}

func (w *DB) nodeForDelete(ctx context.Context, nodeID uuid.UUID) (db.Node, bool, error) {
	node, err := w.DB.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Node{}, false, nil
		}
		return db.Node{}, false, fmt.Errorf("failed to get node %s before DB reconcile delete: %w", nodeID, err)
	}
	return node, true, nil
}
