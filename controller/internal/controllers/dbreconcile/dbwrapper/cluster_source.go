// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type clusterEventSource struct {
	queries        db.ClusterQueries
	filters        db.ClusterFilters
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
	ticker := time.NewTicker(s.resyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.enqueueAll(ctx, enqueue); err != nil {
				s.logger.Warn("Failed to resync DB cluster reconcile source", "error", err)
			}
		}
	}
}

func (s clusterEventSource) enqueueAll(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	clusters, err := s.queries.ListClusters(ctx, s.filters)
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	for _, cluster := range clusters {
		if err := enqueue(ctx, dbreconcile.ClusterKey(cluster.ID)); err != nil {
			return fmt.Errorf("failed to enqueue cluster %s: %w", cluster.ID, err)
		}
	}
	return nil
}
