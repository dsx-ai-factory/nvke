// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"fmt"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type nodeEventSource struct {
	queries db.NodeQueries
	filters db.NodeFilters
}

func (s nodeEventSource) Start(ctx context.Context, enqueue func(context.Context, dbreconcile.Key) error) error {
	dbNodes, err := s.queries.ListNodes(ctx, s.filters)
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, node := range dbNodes {
		if err := enqueue(ctx, dbreconcile.NodeKey(node.ID)); err != nil {
			return fmt.Errorf("failed to enqueue node %s: %w", node.ID, err)
		}
	}

	return nil
}
