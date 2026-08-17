// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
