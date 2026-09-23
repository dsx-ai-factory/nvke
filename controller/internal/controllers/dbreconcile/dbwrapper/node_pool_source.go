// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// NodePoolEventSource is a controller-runtime source for DB node pool changes.
type NodePoolEventSource[T comparable] struct {
	events         chan event.TypedGenericEvent[T]
	done           <-chan struct{}
	database       *DB
	filters        dbfilters.NodePool
	keyMappers     []NodePoolKeyMapper[T]
	resyncInterval time.Duration
	logger         *slog.Logger
	coalesce       *keyCoalescer[T]
}

func NewNodePoolSource(database *DB, filters dbfilters.NodePool, keyMappers ...NodePoolKeyMapper[dbreconcile.Key]) *NodePoolEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodePoolKeyMapper[dbreconcile.Key]{defaultNodePoolKey}
	}
	return NewMappedNodePoolSource(database, filters, keyMappers...)
}

// NewMappedNodePoolSource returns a DB-backed controller-runtime source for node pool changes.
// The source is registered with database while controller-runtime is running it.
func NewMappedNodePoolSource[T comparable](
	database *DB,
	filters dbfilters.NodePool,
	keyMappers ...NodePoolKeyMapper[T],
) *NodePoolEventSource[T] {
	s := &NodePoolEventSource[T]{
		events:         make(chan event.TypedGenericEvent[T], defaultSourceBufferSize),
		database:       database,
		filters:        filters,
		keyMappers:     keyMappers,
		resyncInterval: defaultSourceResyncInterval,
		logger:         database.logger,
	}
	s.coalesce = newKeyCoalescer(defaultSourceCoalesceWindow, s.deliver)
	return s
}

func (s *NodePoolEventSource[T]) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[T]) error {
	s.done = ctx.Done()
	inner := crsource.TypedChannel[T, T](
		s.events,
		handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, key T) []T {
			return []T{key}
		}),
	)
	if err := inner.Start(ctx, queue); err != nil {
		return err
	}

	unregister := s.database.registerNodePoolSource(ctx, s.Enqueue)
	if err := s.enqueueAll(ctx); err != nil {
		unregister()
		return err
	}
	if s.resyncInterval > 0 {
		go s.resync(ctx)
	}

	return nil
}

func (s *NodePoolEventSource[T]) resync(ctx context.Context) {
	for {
		timer := time.NewTimer(wait.Jitter(s.resyncInterval, defaultSourceResyncJitterFactor))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			if err := s.enqueueAll(ctx); err != nil {
				s.logger.Warn("Failed to resync DB node pool reconcile source", "error", err)
			}
		}
	}
}

func (s *NodePoolEventSource[T]) enqueueAll(ctx context.Context) error {
	nodePools, err := s.database.ListNodePools(ctx, db.NodePoolFilters{})
	if err != nil {
		return fmt.Errorf("failed to list node pools: %w", err)
	}

	return s.Enqueue(ctx, nodePools...)
}

func (s *NodePoolEventSource[T]) Enqueue(ctx context.Context, nodePools ...db.NodePool) error {
	seen := map[T]struct{}{}
	var enqueueErrs []error
	for _, nodePool := range nodePools {
		if !s.filters.Matches(nodePool) {
			continue
		}
		for _, keyMapper := range s.keyMappers {
			keys, err := keyMapper(ctx, nodePool)
			if err != nil {
				enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to map node pool %s keys: %w", nodePool.ID, err))
				continue
			}
			for _, key := range keys {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if err := s.enqueueKey(ctx, key); err != nil {
					enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to enqueue node pool %s key %v: %w", nodePool.ID, key, err))
				}
			}
		}
	}

	return errors.Join(enqueueErrs...)
}

func (s *NodePoolEventSource[T]) enqueueKey(ctx context.Context, key T) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, fmt.Errorf("failed to enqueue DB node pool reconcile event"))
	}
	s.coalesce.offer(key)
	return nil
}

func (s *NodePoolEventSource[T]) deliver(key T) {
	select {
	case <-s.done:
	case s.events <- event.TypedGenericEvent[T]{Object: key}:
	}
}
