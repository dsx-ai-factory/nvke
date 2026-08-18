// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestUnit_KeyConstructors(t *testing.T) {
	id := uuid.New()

	type parameters struct {
		key  Key
		kind Kind
	}

	tests := map[string]parameters{
		"cluster": {
			key:  ClusterKey(id),
			kind: KindCluster,
		},
		"node pool": {
			key:  NodePoolKey(id),
			kind: KindNodePool,
		},
		"node set": {
			key:  NodeSetKey(id),
			kind: KindNodeSet,
		},
		"node": {
			key:  NodeKey(id),
			kind: KindNode,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, params.kind, params.key.Kind)
			assert.Equal(t, id, params.key.ID)
		})
	}
}

func TestUnit_KeyString(t *testing.T) {
	id := uuid.MustParse("8cff6eb2-b1dd-4555-898a-6bf849b0b9a4")

	assert.Equal(t, "node_pool/8cff6eb2-b1dd-4555-898a-6bf849b0b9a4", NodePoolKey(id).String())
}
