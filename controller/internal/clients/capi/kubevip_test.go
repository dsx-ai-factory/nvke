// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_UsesFMDSBGPPeerASN(t *testing.T) {
	useFMDS, err := New().UsesFMDSBGPPeerASN(db.NodePool{})
	require.NoError(t, err)
	assert.False(t, useFMDS)
}
