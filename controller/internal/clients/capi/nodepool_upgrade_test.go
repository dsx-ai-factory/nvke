// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_ResolveNodePoolUpgrade(t *testing.T) {
	nodePool := db.NodePool{ID: uuid.New(), KubernetesVersion: "1.31.9"}

	updated, err := New().ResolveNodePoolUpgrade(t.Context(), nil, nodePool, "1.32.0", nil)
	require.NoError(t, err)
	assert.Equal(t, "1.32.0", updated.KubernetesVersion)
	assert.Equal(t, nodePool.ID, updated.ID)
}
