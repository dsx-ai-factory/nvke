// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ApplyNodeSetProvider(t *testing.T) {
	t.Run("missing node pool payload errors", func(t *testing.T) {
		err := New().ApplyNodeSetProvider(db.NodePool{}, &db.NodeSet{})
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})

	t.Run("sets the node set's nico payload from the node pool's", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{Release: "profile-a"})

		newNodeSet := &db.NodeSet{}
		require.NoError(t, New().ApplyNodeSetProvider(nodePool, newNodeSet))

		nicoProvider, err := newNodeSet.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, "profile-a", nicoProvider.Release)
	})
}
