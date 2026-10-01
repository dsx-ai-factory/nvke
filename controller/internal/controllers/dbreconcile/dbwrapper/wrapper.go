// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	defaultSourceResyncInterval     = 15 * time.Minute
	defaultSourceResyncJitterFactor = 0.1
	defaultSourceBufferSize         = 1024
	// A key is delivered at most once per window. Every DB write fans out to
	// these sources, and a write storm on one row otherwise reconciles it at
	// transaction speed (10-30 times a second seen during upgrades and reboots).
	defaultSourceCoalesceWindow = time.Second
)

type clusterSourceRegistration struct {
	id      uuid.UUID
	enqueue func(context.Context, ...db.Cluster) error
}

type nodePoolSourceRegistration struct {
	id      uuid.UUID
	enqueue func(context.Context, ...db.NodePool) error
}

type nodeSetSourceRegistration struct {
	id      uuid.UUID
	enqueue func(context.Context, ...db.NodeSet) error
}

type nodeSourceRegistration struct {
	id      uuid.UUID
	enqueue func(context.Context, ...db.Node) error
}

// DB wraps the controller database and emits DB reconcile events for watched
// rows. DB-backed reconcilers and writer paths that must wake DB reconcile
// sources must use this handle rather than raw db.DB.
type DB struct {
	db.DB

	logger          *slog.Logger
	lock            sync.RWMutex
	clusterSources  map[uuid.UUID]clusterSourceRegistration
	nodeSources     map[uuid.UUID]nodeSourceRegistration
	nodePoolSources map[uuid.UUID]nodePoolSourceRegistration
	nodeSetSources  map[uuid.UUID]nodeSetSourceRegistration
}

func New(database db.DB, logger *slog.Logger) *DB {
	if logger == nil {
		logger = slog.Default()
	}
	return &DB{
		DB:              database,
		logger:          logger,
		clusterSources:  make(map[uuid.UUID]clusterSourceRegistration),
		nodeSources:     make(map[uuid.UUID]nodeSourceRegistration),
		nodePoolSources: make(map[uuid.UUID]nodePoolSourceRegistration),
		nodeSetSources:  make(map[uuid.UUID]nodeSetSourceRegistration),
	}
}

func (w *DB) registerClusterSource(ctx context.Context, enqueue func(context.Context, ...db.Cluster) error) func() {
	id := uuid.New()
	registration := clusterSourceRegistration{
		id:      id,
		enqueue: enqueue,
	}
	w.lock.Lock()
	w.clusterSources[registration.id] = registration
	w.lock.Unlock()

	stop := context.AfterFunc(ctx, func() {
		w.unregisterClusterSource(id)
	})
	return func() {
		stop()
		w.unregisterClusterSource(id)
	}
}

func (w *DB) unregisterClusterSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.clusterSources, id)
}

func (w *DB) registerNodePoolSource(ctx context.Context, enqueue func(context.Context, ...db.NodePool) error) func() {
	id := uuid.New()
	registration := nodePoolSourceRegistration{
		id:      id,
		enqueue: enqueue,
	}
	w.lock.Lock()
	w.nodePoolSources[registration.id] = registration
	w.lock.Unlock()

	stop := context.AfterFunc(ctx, func() {
		w.unregisterNodePoolSource(id)
	})
	return func() {
		stop()
		w.unregisterNodePoolSource(id)
	}
}

func (w *DB) unregisterNodePoolSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.nodePoolSources, id)
}

func (w *DB) registerNodeSetSource(ctx context.Context, enqueue func(context.Context, ...db.NodeSet) error) func() {
	id := uuid.New()
	registration := nodeSetSourceRegistration{
		id:      id,
		enqueue: enqueue,
	}
	w.lock.Lock()
	w.nodeSetSources[registration.id] = registration
	w.lock.Unlock()

	stop := context.AfterFunc(ctx, func() {
		w.unregisterNodeSetSource(id)
	})
	return func() {
		stop()
		w.unregisterNodeSetSource(id)
	}
}

func (w *DB) unregisterNodeSetSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.nodeSetSources, id)
}

func (w *DB) registerNodeSource(ctx context.Context, enqueue func(context.Context, ...db.Node) error) func() {
	id := uuid.New()
	registration := nodeSourceRegistration{
		id:      id,
		enqueue: enqueue,
	}
	w.lock.Lock()
	w.nodeSources[registration.id] = registration
	w.lock.Unlock()

	stop := context.AfterFunc(ctx, func() {
		w.unregisterNodeSource(id)
	})
	return func() {
		stop()
		w.unregisterNodeSource(id)
	}
}

func (w *DB) unregisterNodeSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.nodeSources, id)
}

func (w *DB) CreateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	created, err := w.DB.CreateCluster(ctx, cluster)
	if err != nil {
		return created, err
	}
	if err := w.enqueueClusters(ctx, created); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	previous, err := w.DB.GetCluster(ctx, cluster.ID)
	if err != nil {
		return db.Cluster{}, fmt.Errorf("failed to get cluster before update: %w", err)
	}

	updated, err := w.DB.UpdateCluster(ctx, cluster)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueClusters(ctx, previous, updated); err != nil {
		return updated, err
	}
	return updated, nil
}

func (w *DB) DeleteCluster(ctx context.Context, id uuid.UUID) error {
	if err := w.DB.DeleteCluster(ctx, id); err != nil {
		return err
	}
	return w.enqueueClusters(ctx, db.Cluster{ID: id})
}

func (w *DB) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	created, err := w.DB.CreateNodePool(ctx, nodePool)
	if err != nil {
		return created, err
	}
	if err := w.enqueueNodePools(ctx, created); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	previous, err := w.DB.GetNodePool(ctx, nodePool.ID)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to get node pool before update: %w", err)
	}

	updated, err := w.DB.UpdateNodePool(ctx, nodePool)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueNodePools(ctx, previous, updated); err != nil {
		return updated, err
	}
	return updated, nil
}

func (w *DB) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	deleted, err := w.DB.GetNodePool(ctx, nodePoolID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			if err := w.DB.DeleteNodePool(ctx, nodePoolID); err != nil {
				return err
			}
			return w.enqueueNodePools(ctx, db.NodePool{ID: nodePoolID})
		}
		return fmt.Errorf("failed to get node pool before delete: %w", err)
	}

	if err := w.DB.DeleteNodePool(ctx, nodePoolID); err != nil {
		return err
	}
	return w.enqueueNodePools(ctx, deleted)
}

func (w *DB) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	created, err := w.DB.CreateNodeSet(ctx, nodeSet)
	if err != nil {
		return created, err
	}
	if err := w.enqueueNodeSets(ctx, created); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	previous, err := w.DB.GetNodeSet(ctx, nodeSet.ID)
	if err != nil {
		return db.NodeSet{}, fmt.Errorf("failed to get node set before update: %w", err)
	}

	updated, err := w.DB.UpdateNodeSet(ctx, nodeSet)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueNodeSets(ctx, previous, updated); err != nil {
		return updated, err
	}
	return updated, nil
}

func (w *DB) DeleteNodeSet(ctx context.Context, nodeSetID uuid.UUID) error {
	deleted, err := w.DB.GetNodeSet(ctx, nodeSetID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			if err := w.DB.DeleteNodeSet(ctx, nodeSetID); err != nil {
				return err
			}
			return w.enqueueNodeSets(ctx, db.NodeSet{ID: nodeSetID})
		}
		return fmt.Errorf("failed to get node set before delete: %w", err)
	}

	if err := w.DB.DeleteNodeSet(ctx, nodeSetID); err != nil {
		return err
	}
	return w.enqueueNodeSets(ctx, deleted)
}

func (w *DB) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	created, err := w.DB.CreateNode(ctx, node)
	if err != nil {
		return created, err
	}
	if err := w.enqueueNodes(ctx, created); err != nil {
		return created, err
	}
	return created, nil
}

func (w *DB) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	previous, err := w.DB.GetNode(ctx, node.ID)
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to get node before update: %w", err)
	}

	updated, err := w.DB.UpdateNode(ctx, node)
	if err != nil {
		return updated, err
	}
	if err := w.enqueueNodes(ctx, previous, updated); err != nil {
		return updated, err
	}
	return updated, nil
}

func (w *DB) UpdateNodeTeleportRegistered(ctx context.Context, nodeID uuid.UUID, condition db.Condition) error {
	previous, err := w.DB.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node before Teleport registration update: %w", err)
	}

	if err := w.DB.UpdateNodeTeleportRegistered(ctx, nodeID, condition); err != nil {
		return err
	}

	updated, err := w.DB.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node after Teleport registration update: %w", err)
	}

	return w.enqueueNodes(ctx, previous, updated)
}

func (w *DB) DeleteNode(ctx context.Context, nodeID uuid.UUID) error {
	deleted, err := w.DB.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return w.DB.DeleteNode(ctx, nodeID)
		}
		return fmt.Errorf("failed to get node %s before DB reconcile delete: %w", nodeID, err)
	}

	if err := w.DB.DeleteNode(ctx, nodeID); err != nil {
		return err
	}
	return w.enqueueNodes(ctx, deleted)
}

func (w *DB) RunInTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error {
	tx := &reconcileTx{}
	err := w.DB.RunInTx(ctx, func(ctx context.Context, realTx db.Tx) error {
		tx.Tx = realTx
		return fn(ctx, tx)
	})
	if err != nil {
		return err
	}

	if err := w.enqueueNodes(ctx, tx.nodes...); err != nil {
		w.logger.Warn("Failed to enqueue nodes after committed transaction", "error", err)
	}
	if err := w.enqueueNodePools(ctx, tx.nodePools...); err != nil {
		w.logger.Warn("Failed to enqueue node pools after committed transaction", "error", err)
	}
	if err := w.enqueueNodeSets(ctx, tx.nodeSets...); err != nil {
		w.logger.Warn("Failed to enqueue node sets after committed transaction", "error", err)
	}
	if err := w.enqueueClusters(ctx, tx.clusters...); err != nil {
		w.logger.Warn("Failed to enqueue clusters after committed transaction", "error", err)
	}
	return nil
}

func (w *DB) enqueueClusters(ctx context.Context, clusters ...db.Cluster) error {
	sources := func() []clusterSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		sources := make([]clusterSourceRegistration, 0, len(w.clusterSources))
		for _, source := range w.clusterSources {
			sources = append(sources, source)
		}
		return sources
	}()

	var enqueueErrs []error
	for _, source := range sources {
		if err := source.enqueue(ctx, clusters...); err != nil {
			enqueueErrs = append(enqueueErrs, err)
		}
	}
	return errors.Join(enqueueErrs...)
}

func (w *DB) enqueueNodePools(ctx context.Context, nodePools ...db.NodePool) error {
	sources := func() []nodePoolSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		sources := make([]nodePoolSourceRegistration, 0, len(w.nodePoolSources))
		for _, source := range w.nodePoolSources {
			sources = append(sources, source)
		}
		return sources
	}()

	var enqueueErrs []error
	for _, source := range sources {
		if err := source.enqueue(ctx, nodePools...); err != nil {
			enqueueErrs = append(enqueueErrs, err)
		}
	}
	return errors.Join(enqueueErrs...)
}

func (w *DB) enqueueNodeSets(ctx context.Context, nodeSets ...db.NodeSet) error {
	sources := func() []nodeSetSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		sources := make([]nodeSetSourceRegistration, 0, len(w.nodeSetSources))
		for _, source := range w.nodeSetSources {
			sources = append(sources, source)
		}
		return sources
	}()

	var enqueueErrs []error
	for _, source := range sources {
		if err := source.enqueue(ctx, nodeSets...); err != nil {
			enqueueErrs = append(enqueueErrs, err)
		}
	}
	return errors.Join(enqueueErrs...)
}

func (w *DB) enqueueNodes(ctx context.Context, nodes ...db.Node) error {
	sources := func() []nodeSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		sources := make([]nodeSourceRegistration, 0, len(w.nodeSources))
		for _, source := range w.nodeSources {
			sources = append(sources, source)
		}
		return sources
	}()

	var enqueueErrs []error
	for _, source := range sources {
		if err := source.enqueue(ctx, nodes...); err != nil {
			enqueueErrs = append(enqueueErrs, err)
		}
	}
	return errors.Join(enqueueErrs...)
}
