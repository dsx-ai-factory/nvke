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

type nodeEventSource struct {
	queries        db.NodeQueries
	filters        dbfilters.Node
	keyMappers     []NodeKeyMapper
	resyncInterval time.Duration
	logger         *slog.Logger
}

func (s nodeEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	if err := s.enqueueAll(ctx, enqueue); err != nil {
		return err
	}
	if s.resyncInterval <= 0 {
		return nil
	}

	go s.resync(ctx, enqueue)
	return nil
}

func (s nodeEventSource) resync(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) {
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
				s.logger.Warn("Failed to resync DB node reconcile source", "error", err)
			}
		}
	}
}

func (s nodeEventSource) enqueueAll(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	dbNodes, err := s.queries.ListNodes(ctx, db.NodeFilters{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, node := range dbNodes {
		if !s.filters.Matches(node) {
			continue
		}
		if err := enqueueKeysForNode(ctx, enqueue, node, s.keyMappers...); err != nil {
			return err
		}
	}

	return nil
}

func enqueueKeysForNode(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error, node db.Node, keyMappers ...NodeKeyMapper) error {
	for _, keyMapper := range keyMappers {
		keys := keyMapper(node)
		for _, key := range keys {
			if err := enqueue(ctx, key); err != nil {
				return fmt.Errorf("failed to enqueue node %s key %s: %w", node.ID, key.String(), err)
			}
		}
	}
	return nil
}
