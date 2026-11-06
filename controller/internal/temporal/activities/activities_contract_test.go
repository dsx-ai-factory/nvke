// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

// The env file path and the controller CA bundle path follow the node's agent
// version: the empty version and anything below the cutover get the old forms.
// The CA bundle flag is --nvke-controller-client-ca-bundle for every version.
func TestNodeAgentBootstrapFilesGatedOnAgentVersion(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		agentVersion string
		wantCAPath   string
		wantEnvPath  string
		wantCAFlag   string
	}{
		"empty version is old":          {"", "/etc/nke/controller-ca.crt", "/etc/default/nke-agent", "--nvke-controller-client-ca-bundle"},
		"below the cutover is old":      {"v0.2.0", "/etc/nke/controller-ca.crt", "/etc/default/nke-agent", "--nvke-controller-client-ca-bundle"},
		"at the cutover is new":         {"v0.3.0", "/etc/nvke/controller-ca.crt", "/etc/default/nvke-agent", "--nvke-controller-client-ca-bundle"},
		"cutover git describe is new":   {"v0.3.0-3-gabc1234", "/etc/nvke/controller-ca.crt", "/etc/default/nvke-agent", "--nvke-controller-client-ca-bundle"},
		"unparseable version stays old": {"garbage", "/etc/nke/controller-ca.crt", "/etc/default/nke-agent", "--nvke-controller-client-ca-bundle"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			activities := &Activities{
				cfg: config.Config{
					NodeAgent:                    config.NodeAgentConfig{Port: "15005"},
					NodeAgentExternalMTLSEnabled: true,
				},
				controllerClientCABundle: "public-ca",
			}
			files, err := activities.nodeAgentBootstrapFiles(db.Node{
				NodeAgentExternalMTLSEnabled: true,
				AgentVersion:                 params.agentVersion,
			})
			require.NoError(t, err)
			require.Len(t, files, 2)
			assert.Equal(t, params.wantCAPath, files[0].Path)
			assert.Equal(t, params.wantEnvPath, files[1].Path)
			assert.Contains(t, files[1].Content, " "+params.wantCAFlag+"="+params.wantCAPath)
		})
	}
}

// The controller never passes --controller-audience. The audience moves by the
// agent's own default instead (#2039), and that is deliberate: AGENT_ARGS is
// rendered during CreateCAPIInfraMachineForNode, before the node has booted an
// agent, so the controller cannot know what the image-installed binary accepts.
// Agent v0.2.1 takes --grpc-auth-mode=tls-jwt but predates
// --controller-audience, which arrived in v0.3.0, and an agent handed an
// unknown flag exits during flag.Parse — with no RPC route left to upgrade it.
//
// This builds the node the way the nodeset controller does, through
// PinAgentProfile, so the assertion covers a real pending node rather than one
// with AgentVersion set by hand. It must fail if anything teaches this path to
// send the flag.
func TestNodeAgentBootstrapFilesOnAFreshlyCreatedPendingNode(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		SpiffeEnabled: true,
		NodeAgent: config.NodeAgentConfig{
			Port:                   "15005",
			SpireAttestationMethod: config.SpireAttestationMethodTPM,
			TLSJWT: config.TLSJWTConfig{
				Enabled:                true,
				DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange",
			},
		},
	}

	// The node as the nodeset controller creates it, before any provisioning.
	var node db.Node
	nodes.PinAgentProfile(&node, cfg)
	require.True(t, node.NodeAgentTLSJWTEnabled, "the pinned profile must be the TLS/JWT one this gate is about")
	require.Empty(t, node.AgentVersion, "a pending node has never reported an agent version")

	files, err := (&Activities{cfg: cfg}).nodeAgentBootstrapFiles(node)
	require.NoError(t, err)
	require.Len(t, files, 1)

	// The old form, down to the env file the old-form agent reads.
	assert.Equal(t, "/etc/default/nke-agent", files[0].Path)
	assert.Contains(t, files[0].Content, "--grpc-auth-mode=tls-jwt")
	assert.NotContains(t, files[0].Content, "--controller-audience",
		"the controller never selects the audience; no agent may be handed a flag it predates")
}
