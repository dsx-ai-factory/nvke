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
// version: the empty version and anything below the cutover get the old paths.
func TestNodeAgentBootstrapFilesGatedOnAgentVersion(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		agentVersion string
		wantCAPath   string
		wantEnvPath  string
	}{
		"empty version is old":          {"", "/etc/nke/controller-ca.crt", "/etc/default/nke-agent"},
		"below the cutover is old":      {"v0.2.0", "/etc/nke/controller-ca.crt", "/etc/default/nke-agent"},
		"at the cutover is new":         {"v0.3.0", "/etc/nvke/controller-ca.crt", "/etc/default/nvke-agent"},
		"cutover git describe is new":   {"v0.3.0-3-gabc1234", "/etc/nvke/controller-ca.crt", "/etc/default/nvke-agent"},
		"unparseable version stays old": {"garbage", "/etc/nke/controller-ca.crt", "/etc/default/nke-agent"},
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
			assert.Contains(t, files[1].Content, "--controller-client-ca-bundle="+params.wantCAPath)
		})
	}
}
