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

type nodeSetEventSource struct {
	queries        db.NodeSetQueries
	filters        dbfilters.NodeSet
	keyMappers     []NodeSetKeyMapper
	resyncInterval time.Duration
	logger         *slog.Logger
}

func (s nodeSetEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	if err := s.enqueueAll(ctx, enqueue); err != nil {
		return err
	}
	if s.resyncInterval <= 0 {
		return nil
	}

	go s.resync(ctx, enqueue)
	return nil
}

func (s nodeSetEventSource) resync(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) {
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
				s.logger.Warn("Failed to resync DB node set reconcile source", "error", err)
			}
		}
	}
}

func (s nodeSetEventSource) enqueueAll(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	nodeSets, err := s.queries.ListNodeSets(ctx, db.NodeSetFilters{})
	if err != nil {
		return fmt.Errorf("failed to list node sets: %w", err)
	}

	for _, nodeSet := range nodeSets {
		if !s.filters.Matches(nodeSet) {
			continue
		}
		if err := enqueueKeysForNodeSet(ctx, enqueue, nodeSet, s.keyMappers...); err != nil {
			return err
		}
	}

	return nil
}

func enqueueKeysForNodeSet(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error, nodeSet db.NodeSet, keyMappers ...NodeSetKeyMapper) error {
	for _, keyMapper := range keyMappers {
		keys, err := keyMapper(ctx, nodeSet)
		if err != nil {
			return fmt.Errorf("failed to map node set %s keys: %w", nodeSet.ID, err)
		}
		for _, key := range keys {
			if err := enqueue(ctx, key); err != nil {
				return fmt.Errorf("failed to enqueue node set %s key %s: %w", nodeSet.ID, key.String(), err)
			}
		}
	}
	return nil
}
