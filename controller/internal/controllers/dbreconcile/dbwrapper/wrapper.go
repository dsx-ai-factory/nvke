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

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	defaultSourceResyncInterval     = 15 * time.Minute
	defaultSourceResyncJitterFactor = 0.1
	defaultSourceBufferSize         = 1024
)

type clusterSourceRegistration struct {
	id      uuid.UUID
	done    <-chan struct{}
	enqueue func(context.Context, ...db.Cluster) error
}

type nodePoolSourceRegistration struct {
	id      uuid.UUID
	done    <-chan struct{}
	enqueue func(context.Context, ...db.NodePool) error
}

type nodeSetSourceRegistration struct {
	id      uuid.UUID
	done    <-chan struct{}
	enqueue func(context.Context, ...db.NodeSet) error
}

type nodeSourceRegistration struct {
	id      uuid.UUID
	done    <-chan struct{}
	enqueue func(context.Context, ...db.Node) error
}

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

func (w *DB) ClusterSource(filters dbfilters.Cluster, keyMappers ...ClusterKeyMapper[dbreconcile.Key]) *ClusterEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []ClusterKeyMapper[dbreconcile.Key]{defaultClusterKey}
	}
	return ClusterSource(context.Background(), w, filters, keyMappers...)
}

// ClusterSource returns a DB-backed controller-runtime source for cluster changes.
// The source is registered with database until ctx is canceled.
func ClusterSource[T comparable](ctx context.Context, database *DB, filters dbfilters.Cluster, keyMappers ...ClusterKeyMapper[T]) *ClusterEventSource[T] {
	clusterSource := newClusterSource(ctx, database, database.logger, filters, keyMappers...)

	id := uuid.New()
	database.registerClusterSource(clusterSourceRegistration{
		id:      id,
		done:    ctx.Done(),
		enqueue: clusterSource.Enqueue,
	})
	context.AfterFunc(ctx, func() {
		database.unregisterClusterSource(id)
	})
	return clusterSource
}

func (w *DB) registerClusterSource(registration clusterSourceRegistration) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.clusterSources[registration.id] = registration
}

func (w *DB) unregisterClusterSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.clusterSources, id)
}

func (w *DB) NodePoolSource(filters dbfilters.NodePool, keyMappers ...NodePoolKeyMapper[dbreconcile.Key]) *NodePoolEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodePoolKeyMapper[dbreconcile.Key]{defaultNodePoolKey}
	}
	return NodePoolSource(context.Background(), w, filters, keyMappers...)
}

// NodePoolSource returns a DB-backed controller-runtime source for node pool changes.
// The source is registered with database until ctx is canceled.
func NodePoolSource[T comparable](ctx context.Context, database *DB, filters dbfilters.NodePool, keyMappers ...NodePoolKeyMapper[T]) *NodePoolEventSource[T] {
	nodePoolSource := newNodePoolSource(ctx, database, database.logger, filters, keyMappers...)

	id := uuid.New()
	database.registerNodePoolSource(nodePoolSourceRegistration{
		id:      id,
		done:    ctx.Done(),
		enqueue: nodePoolSource.Enqueue,
	})
	context.AfterFunc(ctx, func() {
		database.unregisterNodePoolSource(id)
	})
	return nodePoolSource
}

func (w *DB) registerNodePoolSource(registration nodePoolSourceRegistration) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodePoolSources[registration.id] = registration
}

func (w *DB) unregisterNodePoolSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.nodePoolSources, id)
}

func (w *DB) NodeSetSource(filters dbfilters.NodeSet, keyMappers ...NodeSetKeyMapper[dbreconcile.Key]) *NodeSetEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodeSetKeyMapper[dbreconcile.Key]{defaultNodeSetKey}
	}
	return NodeSetSource(context.Background(), w, filters, keyMappers...)
}

// NodeSetSource returns a DB-backed controller-runtime source for node set changes.
// The source is registered with database until ctx is canceled.
func NodeSetSource[T comparable](ctx context.Context, database *DB, filters dbfilters.NodeSet, keyMappers ...NodeSetKeyMapper[T]) *NodeSetEventSource[T] {
	nodeSetSource := newNodeSetSource(ctx, database, database.logger, filters, keyMappers...)

	id := uuid.New()
	database.registerNodeSetSource(nodeSetSourceRegistration{
		id:      id,
		done:    ctx.Done(),
		enqueue: nodeSetSource.Enqueue,
	})
	context.AfterFunc(ctx, func() {
		database.unregisterNodeSetSource(id)
	})
	return nodeSetSource
}

func (w *DB) registerNodeSetSource(registration nodeSetSourceRegistration) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodeSetSources[registration.id] = registration
}

func (w *DB) unregisterNodeSetSource(id uuid.UUID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	delete(w.nodeSetSources, id)
}

func (w *DB) NodeSource(filters dbfilters.Node, keyMappers ...NodeKeyMapper[dbreconcile.Key]) *NodeEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodeKeyMapper[dbreconcile.Key]{defaultNodeKey}
	}
	return NodeSource(context.Background(), w, filters, keyMappers...)
}

// NodeSource returns a DB-backed controller-runtime source for node changes.
// The source is registered with database until ctx is canceled. Existing
// process-lifetime callers should use (*DB).NodeSource, which preserves the
// historical background lifetime and dbreconcile.Key queue type.
func NodeSource[T comparable](ctx context.Context, database *DB, filters dbfilters.Node, keyMappers ...NodeKeyMapper[T]) *NodeEventSource[T] {
	nodeSource := newNodeSource(ctx, database, database.logger, filters, keyMappers...)

	id := uuid.New()
	database.registerNodeSource(nodeSourceRegistration{
		id:      id,
		done:    ctx.Done(),
		enqueue: nodeSource.Enqueue,
	})
	context.AfterFunc(ctx, func() {
		database.unregisterNodeSource(id)
	})
	return nodeSource
}

func (w *DB) registerNodeSource(registration nodeSourceRegistration) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodeSources[registration.id] = registration
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

func (w *DB) UpdateClusterNextCertRenewalDue(ctx context.Context, clusterID uuid.UUID, nextDue time.Time) error {
	return w.DB.UpdateClusterNextCertRenewalDue(ctx, clusterID, nextDue)
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

func (w *DB) UpdateNodeForgeDetails(ctx context.Context, nodeID uuid.UUID, forgeInstanceID, forgeMachineID, ip *string) error {
	if forgeInstanceID == nil && forgeMachineID == nil && ip == nil {
		return w.DB.UpdateNodeForgeDetails(ctx, nodeID, forgeInstanceID, forgeMachineID, ip)
	}

	previous, err := w.DB.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node before Forge details update: %w", err)
	}

	if err := w.DB.UpdateNodeForgeDetails(ctx, nodeID, forgeInstanceID, forgeMachineID, ip); err != nil {
		return err
	}

	updated, err := w.DB.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node after Forge details update: %w", err)
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
		if sourceDone(source.done) {
			continue
		}
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
		if sourceDone(source.done) {
			continue
		}
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
		if sourceDone(source.done) {
			continue
		}
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
		if sourceDone(source.done) {
			continue
		}
		if err := source.enqueue(ctx, nodes...); err != nil {
			enqueueErrs = append(enqueueErrs, err)
		}
	}
	return errors.Join(enqueueErrs...)
}

func sourceDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
