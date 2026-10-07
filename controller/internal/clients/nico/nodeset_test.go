// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestClient_NodeSetProviderChanged(t *testing.T) {
	t.Run("missing node pool payload errors", func(t *testing.T) {
		_, err := New().NodeSetProviderChanged(db.NodePool{}, db.NodeSet{})
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})

	t.Run("unchanged when node set already matches", func(t *testing.T) {
		subnetID := uuid.New()
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{OSProfile: "profile-a", SubnetIDs: []uuid.UUID{subnetID}})
		nicoProvider, err := nodePool.NicoProvider()
		require.NoError(t, err)

		nodeSet := db.NodeSet{}
		nodeSet.SetNico(nicoProvider)

		changed, err := New().NodeSetProviderChanged(nodePool, nodeSet)
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("changed when subnet ids differ", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{OSProfile: "profile-a", SubnetIDs: []uuid.UUID{uuid.New()}})

		nodeSet := db.NodeSet{}
		nodeSet.SetNico(db.NicoNodePoolProvider{OSProfile: "profile-a", SubnetIDs: []uuid.UUID{uuid.New()}})

		changed, err := New().NodeSetProviderChanged(nodePool, nodeSet)
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("changed when current node set has no payload yet", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{OSProfile: "profile-a"})

		changed, err := New().NodeSetProviderChanged(nodePool, db.NodeSet{})
		require.NoError(t, err)
		assert.True(t, changed)
	})
}

func TestClient_ApplyNodeSetProvider(t *testing.T) {
	t.Run("missing node pool payload errors", func(t *testing.T) {
		err := New().ApplyNodeSetProvider(db.NodePool{}, &db.NodeSet{})
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})

	t.Run("sets the node set's nico payload from the node pool's", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{OSProfile: "profile-a"})

		newNodeSet := &db.NodeSet{}
		require.NoError(t, New().ApplyNodeSetProvider(nodePool, newNodeSet))

		nicoProvider, err := newNodeSet.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, "profile-a", nicoProvider.OSProfile)
	})
}
