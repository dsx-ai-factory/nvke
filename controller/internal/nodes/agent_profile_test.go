// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

func TestUnit_PinAgentProfile(t *testing.T) {
	t.Parallel()
	old := config.Config{SpiffeEnabled: true, NodeAgent: config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM}}
	newProfile := config.Config{SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true, NodeAgent: config.NodeAgentConfig{
		SpireAttestationMethod: config.SpireAttestationMethodDSXIMDS,
		TLSJWT:                 config.TLSJWTConfig{Enabled: true},
	}}
	var oldNode, newNode db.Node
	nodes.PinAgentProfile(&oldNode, old)
	nodes.PinAgentProfile(&newNode, newProfile)
	require.Equal(t, db.SpireAttestationMethodTPM, oldNode.SpireAttestationMethod)
	require.False(t, oldNode.NodeAgentExternalMTLSEnabled)
	require.False(t, oldNode.NodeAgentTLSJWTEnabled)
	require.Equal(t, db.SpireAttestationMethodDSXIMDS, newNode.SpireAttestationMethod)
	require.True(t, newNode.NodeAgentExternalMTLSEnabled)
	require.True(t, newNode.NodeAgentTLSJWTEnabled)
	require.NoError(t, oldNode.ValidateSecurityProfile())
	require.NoError(t, newNode.ValidateSecurityProfile())
}
