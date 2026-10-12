// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type (
	ClusterKeyMapper[T comparable]  func(context.Context, db.Cluster) ([]T, error)
	NodePoolKeyMapper[T comparable] func(context.Context, db.NodePool) ([]T, error)
	NodeSetKeyMapper[T comparable]  func(context.Context, db.NodeSet) ([]T, error)
	NodeKeyMapper[T comparable]     func(context.Context, db.Node) ([]T, error)
)

func defaultClusterKey(_ context.Context, cluster db.Cluster) ([]dbreconcile.Key, error) {
	if cluster.ID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(cluster.ID)}, nil
}

func defaultNodePoolKey(_ context.Context, nodePool db.NodePool) ([]dbreconcile.Key, error) {
	if nodePool.ID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(nodePool.ID)}, nil
}

func defaultNodeSetKey(_ context.Context, nodeSet db.NodeSet) ([]dbreconcile.Key, error) {
	if nodeSet.ID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodeSetKey(nodeSet.ID)}, nil
}

func defaultNodeKey(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.ID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodeKey(node.ID)}, nil
}
