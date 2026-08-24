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
	dbsource "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/source"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	defaultSourceResyncInterval     = 15 * time.Minute
	defaultSourceResyncJitterFactor = 0.1
)

type Enqueuer interface {
	Enqueue(ctx context.Context, key dbreconcile.Key) error
}

type clusterSourceRegistration struct {
	filters    dbfilters.Cluster
	keyMappers []ClusterKeyMapper
	enqueuer   Enqueuer
}

type nodePoolSourceRegistration struct {
	filters    dbfilters.NodePool
	keyMappers []NodePoolKeyMapper
	enqueuer   Enqueuer
}

type nodeSetSourceRegistration struct {
	filters    dbfilters.NodeSet
	keyMappers []NodeSetKeyMapper
	enqueuer   Enqueuer
}

type nodeSourceRegistration struct {
	filters    dbfilters.Node
	keyMappers []NodeKeyMapper
	enqueuer   Enqueuer
}

type DB struct {
	db.DB

	logger          *slog.Logger
	lock            sync.RWMutex
	clusterSources  []clusterSourceRegistration
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

func (w *DB) ClusterSource(filters dbfilters.Cluster, keyMappers ...ClusterKeyMapper) *dbsource.Source[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []ClusterKeyMapper{defaultClusterKey}
	}
	clusterSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		clusterEventSource{
			queries:        w,
			filters:        filters,
			keyMappers:     keyMappers,
			resyncInterval: defaultSourceResyncInterval,
			logger:         w.logger,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.clusterSources = append(w.clusterSources, clusterSourceRegistration{
		filters:    filters,
		keyMappers: keyMappers,
		enqueuer:   clusterSource,
	})
	return clusterSource
}

func (w *DB) NodePoolSource(filters dbfilters.NodePool, keyMappers ...NodePoolKeyMapper) *dbsource.Source[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodePoolKeyMapper{defaultNodePoolKey}
	}
	nodePoolSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		nodePoolEventSource{
			queries:        w,
			filters:        filters,
			keyMappers:     keyMappers,
			resyncInterval: defaultSourceResyncInterval,
			logger:         w.logger,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodePoolSources = append(w.nodePoolSources, nodePoolSourceRegistration{
		filters:    filters,
		keyMappers: keyMappers,
		enqueuer:   nodePoolSource,
	})
	return nodePoolSource
}

func (w *DB) NodeSetSource(filters dbfilters.NodeSet, keyMappers ...NodeSetKeyMapper) *dbsource.Source[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodeSetKeyMapper{defaultNodeSetKey}
	}
	nodeSetSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		nodeSetEventSource{
			queries:        w,
			filters:        filters,
			keyMappers:     keyMappers,
			resyncInterval: defaultSourceResyncInterval,
			logger:         w.logger,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodeSetSources = append(w.nodeSetSources, nodeSetSourceRegistration{
		filters:    filters,
		keyMappers: keyMappers,
		enqueuer:   nodeSetSource,
	})
	return nodeSetSource
}

func (w *DB) NodeSource(filters dbfilters.Node, keyMappers ...NodeKeyMapper) *dbsource.Source[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodeKeyMapper{defaultNodeKey}
	}
	nodeSource := dbsource.NewSource[dbreconcile.Key](
		dbsource.DefaultBufferSize,
		nodeEventSource{
			queries:        w,
			filters:        filters,
			keyMappers:     keyMappers,
			resyncInterval: defaultSourceResyncInterval,
			logger:         w.logger,
		},
	)
	w.lock.Lock()
	defer w.lock.Unlock()
	w.nodeSources = append(w.nodeSources, nodeSourceRegistration{
		filters:    filters,
		keyMappers: keyMappers,
		enqueuer:   nodeSource,
	})
	return nodeSource
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
		return append([]clusterSourceRegistration(nil), w.clusterSources...)
	}()

	for _, source := range sources {
		seen := map[dbreconcile.Key]struct{}{}
		for _, cluster := range clusters {
			if !source.filters.Matches(cluster) {
				continue
			}
			if err := enqueueKeys(ctx, source.enqueuer, seen, keysForCluster(cluster, source.keyMappers...)); err != nil {
				return fmt.Errorf("failed to enqueue keys for cluster %s: %w", cluster.ID, err)
			}
		}
	}
	return nil
}

func (w *DB) enqueueNodePools(ctx context.Context, nodePools ...db.NodePool) error {
	sources := func() []nodePoolSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		return append([]nodePoolSourceRegistration(nil), w.nodePoolSources...)
	}()

	for _, source := range sources {
		seen := map[dbreconcile.Key]struct{}{}
		for _, nodePool := range nodePools {
			if !source.filters.Matches(nodePool) {
				continue
			}
			if err := enqueueKeys(ctx, source.enqueuer, seen, keysForNodePool(nodePool, source.keyMappers...)); err != nil {
				return fmt.Errorf("failed to enqueue keys for node pool %s: %w", nodePool.ID, err)
			}
		}
	}
	return nil
}

func (w *DB) enqueueNodeSets(ctx context.Context, nodeSets ...db.NodeSet) error {
	sources := func() []nodeSetSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		return append([]nodeSetSourceRegistration(nil), w.nodeSetSources...)
	}()

	for _, source := range sources {
		seen := map[dbreconcile.Key]struct{}{}
		for _, nodeSet := range nodeSets {
			if !source.filters.Matches(nodeSet) {
				continue
			}
			if err := enqueueKeys(ctx, source.enqueuer, seen, keysForNodeSet(nodeSet, source.keyMappers...)); err != nil {
				return fmt.Errorf("failed to enqueue keys for node set %s: %w", nodeSet.ID, err)
			}
		}
	}
	return nil
}

func (w *DB) enqueueNodes(ctx context.Context, nodes ...db.Node) error {
	sources := func() []nodeSourceRegistration {
		w.lock.RLock()
		defer w.lock.RUnlock()
		return append([]nodeSourceRegistration(nil), w.nodeSources...)
	}()

	var enqueueErrs []error
	for _, source := range sources {
		seen := map[dbreconcile.Key]struct{}{}
		for _, node := range nodes {
			if !source.filters.Matches(node) {
				continue
			}
			if err := enqueueKeys(ctx, source.enqueuer, seen, keysForNode(node, source.keyMappers...)); err != nil {
				enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to enqueue keys for node %s: %w", node.ID, err))
			}
		}
	}
	return errors.Join(enqueueErrs...)
}

func enqueueKeys(ctx context.Context, enqueuer Enqueuer, seen map[dbreconcile.Key]struct{}, keys []dbreconcile.Key) error {
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := enqueuer.Enqueue(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func keysForCluster(cluster db.Cluster, keyMappers ...ClusterKeyMapper) []dbreconcile.Key {
	keys := make([]dbreconcile.Key, 0, len(keyMappers))
	for _, keyMapper := range keyMappers {
		keys = append(keys, keyMapper(cluster)...)
	}
	return keys
}

func keysForNodePool(nodePool db.NodePool, keyMappers ...NodePoolKeyMapper) []dbreconcile.Key {
	keys := make([]dbreconcile.Key, 0, len(keyMappers))
	for _, keyMapper := range keyMappers {
		keys = append(keys, keyMapper(nodePool)...)
	}
	return keys
}

func keysForNodeSet(nodeSet db.NodeSet, keyMappers ...NodeSetKeyMapper) []dbreconcile.Key {
	keys := make([]dbreconcile.Key, 0, len(keyMappers))
	for _, keyMapper := range keyMappers {
		keys = append(keys, keyMapper(nodeSet)...)
	}
	return keys
}

func keysForNode(node db.Node, keyMappers ...NodeKeyMapper) []dbreconcile.Key {
	keys := make([]dbreconcile.Key, 0, len(keyMappers))
	for _, keyMapper := range keyMappers {
		keys = append(keys, keyMapper(node)...)
	}
	return keys
}
