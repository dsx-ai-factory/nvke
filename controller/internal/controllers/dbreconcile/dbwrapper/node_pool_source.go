// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"fmt"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type nodePoolEventSource struct {
	queries db.NodePoolQueries
	filters db.NodePoolFilters
}

func (s nodePoolEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	nodePools, err := s.queries.ListNodePools(ctx, s.filters)
	if err != nil {
		return fmt.Errorf("failed to list node pools: %w", err)
	}

	for _, nodePool := range nodePools {
		if err := enqueue(ctx, dbreconcile.NodePoolKey(nodePool.ID)); err != nil {
			return fmt.Errorf("failed to enqueue node pool %s: %w", nodePool.ID, err)
		}
	}

	return nil
}
