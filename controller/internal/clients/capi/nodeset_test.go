// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"
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
