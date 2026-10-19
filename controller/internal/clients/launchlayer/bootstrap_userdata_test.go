// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func testDeps(cfg config.Config, trustDomain, trustBundle string) shared.MachineCreateDeps {
	return shared.MachineCreateDeps{
		Config: cfg,
		NodeAgentBootstrapFiles: func(db.Node) ([]db.CloudInitFile, error) {
			return []db.CloudInitFile{{Path: "/etc/default/nke-agent", Content: "AGENT_ARGS=\"\"\n"}}, nil
		},
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return trustDomain, trustBundle, nil
		},
	}
}

// The template has no join_token stanza or NodeAttestor to fall back on, so
// CreateMachine's rejection is what keeps a misconfigured deployment from
// handing a node an attestor-less agent config.
func TestSpireAgentConfigTemplate_HasNoJoinTokenAttestor(t *testing.T) {
	t.Parallel()

	assert.NotContains(t, spireAgentConfigTemplateText, "join_token")
}

func TestRenderBootstrapUserData_TPMAttestor(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		SpiffeEnabled:      true,
		SpireServerAddress: "spire.example.org",
		SpireServerPort:    "9091",
		NodeAgent:          config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
	}
	node := db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM}

	userData, err := (&Client{}).renderBootstrapUserData(context.Background(), testDeps(cfg, "example.org", "bundle"), node)
	require.NoError(t, err)

	assert.Contains(t, userData, "#cloud-config\n")
	assert.Contains(t, userData, `NodeAttestor "tpm"`)
	assert.NotContains(t, userData, `NodeAttestor "join_token"`)
	assert.NotContains(t, userData, `NodeAttestor "dsx_imds"`)
	var parsed map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(userData), &parsed))
}

func TestRenderBootstrapUserData_DSXIMDSAttestor(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		SpiffeEnabled:      true,
		SpireServerAddress: "spire.example.org",
		SpireServerPort:    "9091",
		NodeAgent: config.NodeAgentConfig{
			SpireAttestationMethod: config.SpireAttestationMethodDSXIMDS,
			DSXImds:                config.DSXImdsConfig{Endpoint: "http://192.0.2.10"},
		},
	}
	node := db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS}

	userData, err := (&Client{}).renderBootstrapUserData(context.Background(), testDeps(cfg, "example.org", "bundle"), node)
	require.NoError(t, err)

	assert.Contains(t, userData, `NodeAttestor "dsx_imds"`)
	assert.Contains(t, userData, `endpoint = "http://192.0.2.10"`)
	assert.NotContains(t, userData, `NodeAttestor "join_token"`)
	assert.NotContains(t, userData, `NodeAttestor "tpm"`)
}

func TestRenderBootstrapUserData_SpiffeDisabledSkipsSpireFiles(t *testing.T) {
	t.Parallel()

	cfg := config.Config{NodeAgent: config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM}}
	node := db.Node{SpiffeEnabled: false, SpireAttestationMethod: db.SpireAttestationMethodTPM}

	userData, err := (&Client{}).renderBootstrapUserData(context.Background(), testDeps(cfg, "", ""), node)
	require.NoError(t, err)

	assert.NotContains(t, userData, "/etc/spire-agent/agent.conf")
	assert.NotContains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
}
