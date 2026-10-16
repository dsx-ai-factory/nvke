// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_NodeMachineIDLabelValue(t *testing.T) {
	node := db.Node{}
	node.SetNico(db.NicoNodeProvider{ForgeMachineID: "machine-abc123"})

	value, err := New().NodeMachineIDLabelValue(node)
	require.NoError(t, err)
	require.Empty(t, value, "CAPI nodes never carry a machine-ID label")
}
