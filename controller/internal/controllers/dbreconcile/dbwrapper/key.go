// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type (
	ClusterKeyMapper  func(context.Context, db.Cluster) ([]dbreconcile.Key, error)
	NodePoolKeyMapper func(context.Context, db.NodePool) ([]dbreconcile.Key, error)
	NodeSetKeyMapper  func(context.Context, db.NodeSet) ([]dbreconcile.Key, error)
	NodeKeyMapper     func(context.Context, db.Node) ([]dbreconcile.Key, error)
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
