// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile

import (
	"fmt"

	"github.com/google/uuid"
)

type Kind string

const (
	KindCluster  Kind = "cluster"
	KindNodePool Kind = "node_pool"
	KindNodeSet  Kind = "node_set"
	KindNode     Kind = "node"
)

type Key struct {
	Kind Kind
	ID   uuid.UUID
}

func (k Key) String() string {
	return fmt.Sprintf("%s/%s", k.Kind, k.ID)
}

func ClusterKey(id uuid.UUID) Key {
	return Key{Kind: KindCluster, ID: id}
}

func NodePoolKey(id uuid.UUID) Key {
	return Key{Kind: KindNodePool, ID: id}
}

func NodeSetKey(id uuid.UUID) Key {
	return Key{Kind: KindNodeSet, ID: id}
}

func NodeKey(id uuid.UUID) Key {
	return Key{Kind: KindNode, ID: id}
}
