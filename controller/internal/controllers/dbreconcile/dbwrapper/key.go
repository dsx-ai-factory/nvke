// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type (
	ClusterKeyMapper  func(db.Cluster) []dbreconcile.Key
	NodePoolKeyMapper func(db.NodePool) []dbreconcile.Key
	NodeSetKeyMapper  func(db.NodeSet) []dbreconcile.Key
	NodeKeyMapper     func(db.Node) []dbreconcile.Key
)

func defaultClusterKey(cluster db.Cluster) []dbreconcile.Key {
	if cluster.ID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(cluster.ID)}
}

func defaultNodePoolKey(nodePool db.NodePool) []dbreconcile.Key {
	if nodePool.ID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(nodePool.ID)}
}

func defaultNodeSetKey(nodeSet db.NodeSet) []dbreconcile.Key {
	if nodeSet.ID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.NodeSetKey(nodeSet.ID)}
}

func defaultNodeKey(node db.Node) []dbreconcile.Key {
	if node.ID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.NodeKey(node.ID)}
}
