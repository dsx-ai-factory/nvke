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

// ClusterEventSource is a controller-runtime source for DB cluster changes.
type ClusterEventSource[T comparable] struct {
	events         chan event.TypedGenericEvent[T]
	done           <-chan struct{}
	database       *DB
	filters        dbfilters.Cluster
	keyMappers     []ClusterKeyMapper[T]
	resyncInterval time.Duration
	logger         *slog.Logger
	coalesce       *keyCoalescer[T]
}

func NewClusterSource(database *DB, filters dbfilters.Cluster, keyMappers ...ClusterKeyMapper[dbreconcile.Key]) *ClusterEventSource[dbreconcile.Key] {
	if len(keyMappers) == 0 {
		keyMappers = []ClusterKeyMapper[dbreconcile.Key]{defaultClusterKey}
	}
	return NewMappedClusterSource(database, filters, keyMappers...)
}

// NewMappedClusterSource returns a DB-backed controller-runtime source for cluster changes.
// The source is registered with database while controller-runtime is running it.
func NewMappedClusterSource[T comparable](
	database *DB,
	filters dbfilters.Cluster,
	keyMappers ...ClusterKeyMapper[T],
) *ClusterEventSource[T] {
	s := &ClusterEventSource[T]{
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

func (s *ClusterEventSource[T]) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[T]) error {
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

	unregister := s.database.registerClusterSource(ctx, s.Enqueue)
	if err := s.enqueueAll(ctx); err != nil {
		unregister()
		return err
	}
	if s.resyncInterval > 0 {
		go s.resync(ctx)
	}

	return nil
}

func (s *ClusterEventSource[T]) resync(ctx context.Context) {
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
				s.logger.Warn("Failed to resync DB cluster reconcile source", "error", err)
			}
		}
	}
}

func (s *ClusterEventSource[T]) enqueueAll(ctx context.Context) error {
	clusters, err := s.database.ListClusters(ctx, db.ClusterFilters{})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	return s.Enqueue(ctx, clusters...)
}

func (s *ClusterEventSource[T]) Enqueue(ctx context.Context, clusters ...db.Cluster) error {
	seen := map[T]struct{}{}
	var enqueueErrs []error
	for _, cluster := range clusters {
		if !s.filters.Matches(cluster) {
			continue
		}
		for _, keyMapper := range s.keyMappers {
			keys, err := keyMapper(ctx, cluster)
			if err != nil {
				enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to map cluster %s keys: %w", cluster.ID, err))
				continue
			}
			for _, key := range keys {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if err := s.enqueueKey(ctx, key); err != nil {
					enqueueErrs = append(enqueueErrs, fmt.Errorf("failed to enqueue cluster %s key %v: %w", cluster.ID, key, err))
				}
			}
		}
	}
	return errors.Join(enqueueErrs...)
}

func (s *ClusterEventSource[T]) enqueueKey(ctx context.Context, key T) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, fmt.Errorf("failed to enqueue DB cluster reconcile event"))
	}
	s.coalesce.offer(key)
	return nil
}

func (s *ClusterEventSource[T]) deliver(key T) {
	select {
	case <-s.done:
	case s.events <- event.TypedGenericEvent[T]{Object: key}:
	}
}
