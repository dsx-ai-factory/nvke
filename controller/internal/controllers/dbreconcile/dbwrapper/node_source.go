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

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// NodeEventSource is a controller-runtime source for DB node changes.
type NodeEventSource[T comparable] struct {
	events         chan event.TypedGenericEvent[T]
	done           <-chan struct{}
	database       *DB
	filters        dbfilters.Node
	keyMappers     []NodeKeyMapper[T]
	resyncInterval time.Duration
	logger         *slog.Logger
}

func NewNodeSource(database *DB, filters dbfilters.Node, keyMappers ...NodeKeyMapper[dbreconcile.Key]) *NodeEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []NodeKeyMapper[dbreconcile.Key]{defaultNodeKey}
	}
	return NewMappedNodeSource(database, filters, keyMappers...)
}

// NewMappedNodeSource returns a DB-backed controller-runtime source for node changes.
// The source is registered with database while controller-runtime is running it.
func NewMappedNodeSource[T comparable](
	database *DB,
	filters dbfilters.Node,
	keyMappers ...NodeKeyMapper[T],
) *NodeEventSource[T] {
	return &NodeEventSource[T]{
		events:         make(chan event.TypedGenericEvent[T], defaultSourceBufferSize),
		database:       database,
		filters:        filters,
		keyMappers:     keyMappers,
		resyncInterval: defaultSourceResyncInterval,
		logger:         database.logger,
	}
}

func (s *NodeEventSource[T]) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[T]) error {
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

	unregister := s.database.registerNodeSource(ctx, s.Enqueue)
	if err := s.enqueueAll(ctx); err != nil {
		unregister()
		return err
	}
	if s.resyncInterval > 0 {
		go s.resync(ctx)
	}

	return nil
}

func (s *NodeEventSource[T]) resync(ctx context.Context) {
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
				s.logger.Warn("Failed to resync DB node reconcile source", "error", err)
			}
		}
	}
}

func (s *NodeEventSource[T]) enqueueAll(ctx context.Context) error {
	dbNodes, err := s.database.ListNodes(ctx, db.NodeFilters{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	return s.Enqueue(ctx, dbNodes...)
}

func (s *NodeEventSource[T]) Enqueue(ctx context.Context, nodes ...db.Node) error {
	seen := map[T]struct{}{}
	var enqueueErrs []error
	for _, node := range nodes {
		if !s.filters.Matches(node) {
			continue
		}
		for _, keyMapper := range s.keyMappers {
			keys, err := keyMapper(ctx, node)
			if err != nil {
				enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to map node %s keys: %w", node.ID, err))
				continue
			}
			for _, key := range keys {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if err := s.enqueueKey(ctx, key); err != nil {
					enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to enqueue node %s key %v: %w", node.ID, key, err))
				}
			}
		}
	}
	return errors.Join(enqueueErrs...)
}

func (s *NodeEventSource[T]) enqueueKey(ctx context.Context, key T) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, fmt.Errorf("failed to enqueue DB node reconcile event"))
	}
	select {
	case <-s.done:
		return fmt.Errorf("failed to enqueue DB node reconcile event: source stopped")
	case s.events <- event.TypedGenericEvent[T]{Object: key}:
		return nil
	case <-ctx.Done():
		return errors.Join(ctx.Err(), fmt.Errorf("failed to enqueue DB node reconcile event"))
	}
}
