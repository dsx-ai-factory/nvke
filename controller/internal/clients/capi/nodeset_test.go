// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"
	"testing"

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
		nodePool := db.NodePool{}
		nodePool.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{"kind":"Template"}`)})
		capiProvider, err := nodePool.CAPIProvider()
		require.NoError(t, err)

		nodeSet := db.NodeSet{}
		nodeSet.SetCapi(capiProvider)

		changed, err := New().NodeSetProviderChanged(nodePool, nodeSet)
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("changed when template differs", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{"kind":"NewTemplate"}`)})

		nodeSet := db.NodeSet{}
		nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{"kind":"OldTemplate"}`)})

		changed, err := New().NodeSetProviderChanged(nodePool, nodeSet)
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("changed when current node set has no payload yet", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{"kind":"Template"}`)})

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

	t.Run("sets the node set's capi payload from the node pool's", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{"kind":"Template"}`)})

		newNodeSet := &db.NodeSet{}
		require.NoError(t, New().ApplyNodeSetProvider(nodePool, newNodeSet))

		capiProvider, err := newNodeSet.CAPIProvider()
		require.NoError(t, err)
		assert.JSONEq(t, `{"kind":"Template"}`, string(capiProvider.InfraMachineTemplate))
	})
}
