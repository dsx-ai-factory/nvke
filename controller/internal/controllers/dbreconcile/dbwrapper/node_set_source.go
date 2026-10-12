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

	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// NodeSetEventSource is a controller-runtime source for DB node set changes.
type NodeSetEventSource[T comparable] struct {
	events         chan event.TypedGenericEvent[T]
	done           <-chan struct{}
	database       *DB
	filters        dbfilters.NodeSet
	keyMappers     []NodeSetKeyMapper[T]
	resyncInterval time.Duration
	logger         *slog.Logger
	coalesce       *keyCoalescer[T]
}

func NewNodeSetSource(database *DB, filters dbfilters.NodeSet, keyMappers ...NodeSetKeyMapper[dbreconcile.Key]) *NodeSetEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodeSetKeyMapper[dbreconcile.Key]{defaultNodeSetKey}
	}
	return NewMappedNodeSetSource(database, filters, keyMappers...)
}

// NewMappedNodeSetSource returns a DB-backed controller-runtime source for node set changes.
// The source is registered with database while controller-runtime is running it.
func NewMappedNodeSetSource[T comparable](
	database *DB,
	filters dbfilters.NodeSet,
	keyMappers ...NodeSetKeyMapper[T],
) *NodeSetEventSource[T] {
	s := &NodeSetEventSource[T]{
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

func (s *NodeSetEventSource[T]) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[T]) error {
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

	unregister := s.database.registerNodeSetSource(ctx, s.Enqueue)
	if err := s.enqueueAll(ctx); err != nil {
		unregister()
		return err
	}
	if s.resyncInterval > 0 {
		go s.resync(ctx)
	}

	return nil
}

func (s *NodeSetEventSource[T]) resync(ctx context.Context) {
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
				s.logger.Warn("Failed to resync DB node set reconcile source", "error", err)
			}
		}
	}
}

func (s *NodeSetEventSource[T]) enqueueAll(ctx context.Context) error {
	nodeSets, err := s.database.ListNodeSets(ctx, db.NodeSetFilters{})
	if err != nil {
		return fmt.Errorf("failed to list node sets: %w", err)
	}

	return s.Enqueue(ctx, nodeSets...)
}

func (s *NodeSetEventSource[T]) Enqueue(ctx context.Context, nodeSets ...db.NodeSet) error {
	seen := map[T]struct{}{}
	var enqueueErrs []error
	for _, nodeSet := range nodeSets {
		if !s.filters.Matches(nodeSet) {
			continue
		}
		for _, keyMapper := range s.keyMappers {
			keys, err := keyMapper(ctx, nodeSet)
			if err != nil {
				enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to map node set %s keys: %w", nodeSet.ID, err))
				continue
			}
			for _, key := range keys {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if err := s.enqueueKey(ctx, key); err != nil {
					enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to enqueue node set %s key %v: %w", nodeSet.ID, key, err))
				}
			}
		}
	}

	return errors.Join(enqueueErrs...)
}

func (s *NodeSetEventSource[T]) enqueueKey(ctx context.Context, key T) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, fmt.Errorf("failed to enqueue DB node set reconcile event"))
	}
	s.coalesce.offer(key)
	return nil
}

func (s *NodeSetEventSource[T]) deliver(key T) {
	select {
	case <-s.done:
	case s.events <- event.TypedGenericEvent[T]{Object: key}:
	}
}
