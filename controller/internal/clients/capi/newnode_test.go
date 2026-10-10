// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestClient_ApplyNodePoolNodePayload(t *testing.T) {
	node := db.Node{}
	require.NoError(t, New().ApplyNodePoolNodePayload(db.NodePool{}, &node))
	_, err := node.NicoProvider()
	require.Error(t, err, "CAPI never stamps a Nico payload")
}

func TestClient_ApplyPlaceholderNodePayload(t *testing.T) {
	node := db.Node{}
	New().ApplyPlaceholderNodePayload(&node)
	_, err := node.NicoProvider()
	require.Error(t, err, "CAPI never stamps a Nico payload")
}
