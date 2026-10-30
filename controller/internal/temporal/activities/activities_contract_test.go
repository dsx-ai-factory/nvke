// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
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

// The JWT audience follows the same agent-version gate, but asymmetrically: a
// new-form agent is told to request the renamed audience, and an older one is
// sent no flag at all, because it predates --controller-audience and would
// refuse to start. Without the flag it keeps requesting the legacy audience,
// which the controller still accepts.
func TestNodeAgentBootstrapFilesSelectsAudienceOnAgentVersion(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		agentVersion string
		tlsJWT       bool
		wantAudience string
	}{
		"empty version gets no audience flag":     {"", true, ""},
		"below the cutover gets no audience flag": {"v0.2.0", true, ""},
		"unparseable version gets no flag":        {"garbage", true, ""},
		"at the cutover selects the rename":       {"v0.3.0", true, "nvke-controller"},
		"cutover describe output selects rename":  {"v0.3.0-3-gabc1234", true, "nvke-controller"},
		"later version selects the rename":        {"v1.0.0", true, "nvke-controller"},
		"no audience flag without TLS/JWT":        {"v0.3.0", false, ""},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			activities := &Activities{
				cfg: config.Config{
					NodeAgent: config.NodeAgentConfig{
						Port:   "15005",
						TLSJWT: config.TLSJWTConfig{Enabled: params.tlsJWT, DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange"},
					},
				},
			}
			files, err := activities.nodeAgentBootstrapFiles(db.Node{
				NodeAgentTLSJWTEnabled: params.tlsJWT,
				SpiffeEnabled:          true,
				AgentVersion:           params.agentVersion,
			})
			require.NoError(t, err)
			require.Len(t, files, 1)
			if params.wantAudience == "" {
				assert.NotContains(t, files[0].Content, "--controller-audience")
				return
			}
			assert.Contains(t, files[0].Content, "--controller-audience="+params.wantAudience)
		})
	}
}
