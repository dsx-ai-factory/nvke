// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ApplyNodePoolNodePayload(t *testing.T) {
	t.Run("stamps the node pool's pinned instance type", func(t *testing.T) {
		instanceTypeID := uuid.New()
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{InstanceTypeID: instanceTypeID})

		node := db.Node{}
		require.NoError(t, New().ApplyNodePoolNodePayload(nodePool, &node))

		nicoProvider, err := node.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, instanceTypeID, nicoProvider.InstanceTypeID)
	})

	t.Run("missing node pool payload errors", func(t *testing.T) {
		node := db.Node{}
		err := New().ApplyNodePoolNodePayload(db.NodePool{}, &node)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})
}

func TestClient_ApplyPlaceholderNodePayload(t *testing.T) {
	node := db.Node{}
	New().ApplyPlaceholderNodePayload(&node)

	nicoProvider, err := node.NicoProvider()
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, nicoProvider.InstanceTypeID)
}
