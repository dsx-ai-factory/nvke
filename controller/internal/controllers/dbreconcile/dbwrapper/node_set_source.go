// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"fmt"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type nodeSetEventSource struct {
	queries db.NodeSetQueries
	filters db.NodeSetFilters
}

func (s nodeSetEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	nodeSets, err := s.queries.ListNodeSets(ctx, s.filters)
	if err != nil {
		return fmt.Errorf("failed to list node sets: %w", err)
	}

	for _, nodeSet := range nodeSets {
		if err := enqueue(ctx, dbreconcile.NodeSetKey(nodeSet.ID)); err != nil {
			return fmt.Errorf("failed to enqueue node set %s: %w", nodeSet.ID, err)
		}
	}

	return nil
}
