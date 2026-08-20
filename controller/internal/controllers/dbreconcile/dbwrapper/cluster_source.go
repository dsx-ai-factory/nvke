// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type clusterEventSource struct {
	queries        db.ClusterQueries
	filters        dbfilters.Cluster
	keyMappers     []ClusterKeyMapper
	resyncInterval time.Duration
	logger         *slog.Logger
}

func (s clusterEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	if err := s.enqueueAll(ctx, enqueue); err != nil {
		return err
	}
	if s.resyncInterval <= 0 {
		return nil
	}

	go s.resync(ctx, enqueue)
	return nil
}

func (s clusterEventSource) resync(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) {
	for {
		timer := time.NewTimer(wait.Jitter(s.resyncInterval, defaultSourceResyncJitterFactor))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			if err := s.enqueueAll(ctx, enqueue); err != nil {
				s.logger.Warn("Failed to resync DB cluster reconcile source", "error", err)
			}
		}
	}
}

func (s clusterEventSource) enqueueAll(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	clusters, err := s.queries.ListClusters(ctx, db.ClusterFilters{})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	for _, cluster := range clusters {
		if !s.filters.Matches(cluster) {
			continue
		}
		if err := enqueueKeysForCluster(ctx, enqueue, cluster, s.keyMappers...); err != nil {
			return err
		}
	}
	return nil
}

func enqueueKeysForCluster(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error, cluster db.Cluster, keyMappers ...ClusterKeyMapper) error {
	for _, keyMapper := range keyMappers {
		keys := keyMapper(cluster)
		for _, key := range keys {
			if err := enqueue(ctx, key); err != nil {
				return fmt.Errorf("failed to enqueue cluster %s key %s: %w", cluster.ID, key.String(), err)
			}
		}
	}
	return nil
}
