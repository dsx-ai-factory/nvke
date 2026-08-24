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

type nodePoolEventSource struct {
	queries        db.NodePoolQueries
	filters        dbfilters.NodePool
	keyMappers     []NodePoolKeyMapper
	resyncInterval time.Duration
	logger         *slog.Logger
}

func (s nodePoolEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	if err := s.enqueueAll(ctx, enqueue); err != nil {
		return err
	}
	if s.resyncInterval <= 0 {
		return nil
	}

	go s.resync(ctx, enqueue)
	return nil
}

func (s nodePoolEventSource) resync(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) {
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
				s.logger.Warn("Failed to resync DB node pool reconcile source", "error", err)
			}
		}
	}
}

func (s nodePoolEventSource) enqueueAll(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	nodePools, err := s.queries.ListNodePools(ctx, db.NodePoolFilters{})
	if err != nil {
		return fmt.Errorf("failed to list node pools: %w", err)
	}

	for _, nodePool := range nodePools {
		if !s.filters.Matches(nodePool) {
			continue
		}
		if err := enqueueKeysForNodePool(ctx, enqueue, nodePool, s.keyMappers...); err != nil {
			return err
		}
	}

	return nil
}

func enqueueKeysForNodePool(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error, nodePool db.NodePool, keyMappers ...NodePoolKeyMapper) error {
	for _, keyMapper := range keyMappers {
		keys, err := keyMapper(ctx, nodePool)
		if err != nil {
			return fmt.Errorf("failed to map node pool %s keys: %w", nodePool.ID, err)
		}
		for _, key := range keys {
			if err := enqueue(ctx, key); err != nil {
				return fmt.Errorf("failed to enqueue node pool %s key %s: %w", nodePool.ID, key.String(), err)
			}
		}
	}
	return nil
}
