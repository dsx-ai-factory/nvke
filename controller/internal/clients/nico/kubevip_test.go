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

func TestClient_UsesFMDSBGPPeerASN(t *testing.T) {
	t.Run("with VPC prefixes", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{VpcPrefixIDs: []uuid.UUID{uuid.New()}})

		useFMDS, err := New().UsesFMDSBGPPeerASN(nodePool)
		require.NoError(t, err)
		assert.True(t, useFMDS)
	})

	t.Run("without VPC prefixes", func(t *testing.T) {
		nodePool := db.NodePool{}
		nodePool.SetNico(db.NicoNodePoolProvider{})

		useFMDS, err := New().UsesFMDSBGPPeerASN(nodePool)
		require.NoError(t, err)
		assert.False(t, useFMDS)
	})

	t.Run("missing nico payload errors", func(t *testing.T) {
		_, err := New().UsesFMDSBGPPeerASN(db.NodePool{})
		require.Error(t, err)
	})
}
