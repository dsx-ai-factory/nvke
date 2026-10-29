// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestUnit_NodeAgentBootstrapFiles(t *testing.T) {
	t.Parallel()

	type parameters struct {
		tlsJWT       bool
		externalMTLS bool
		spiffe       bool
		wantArgs     string
	}
	tests := map[string]parameters{
		"SPIFFE without JWT": {
			spiffe:   true,
			wantArgs: "AGENT_ARGS=\"--secure-server-port 15005\"\n",
		},
		"SPIFFE with JWT": {
			tlsJWT:   true,
			spiffe:   true,
			wantArgs: "AGENT_ARGS=\"--secure-server-port 15005 --grpc-auth-mode=tls-jwt --dsx-identity-exchange-url=https://identity.example.test/v1/exchange\"\n",
		},
		"external mTLS without JWT": {
			externalMTLS: true,
			wantArgs:     "AGENT_ARGS=\"--secure-server-port 15005 --nvke-controller-client-ca-bundle=/etc/nke/controller-ca.crt\"\n",
		},
		"external mTLS with JWT": {
			tlsJWT:       true,
			externalMTLS: true,
			wantArgs:     "AGENT_ARGS=\"--secure-server-port 15005 --nvke-controller-client-ca-bundle=/etc/nke/controller-ca.crt --grpc-auth-mode=tls-jwt --dsx-identity-exchange-url=https://identity.example.test/v1/exchange\"\n",
		},
		"neither SPIFFE nor external mTLS": {
			wantArgs: "AGENT_ARGS=\"--secure-server-port 15005 --disable-mtls\"\n",
		},
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
					NodeAgentExternalMTLSEnabled: params.externalMTLS,
				},
				controllerClientCABundle: "public-ca",
			}
			files, err := activities.nodeAgentBootstrapFiles(db.Node{
				NodeAgentExternalMTLSEnabled: params.externalMTLS,
				NodeAgentTLSJWTEnabled:       params.tlsJWT,
				SpiffeEnabled:                params.spiffe,
			})
			require.NoError(t, err)
			wantFileCount := 1
			if params.externalMTLS {
				wantFileCount++
			}
			require.Len(t, files, wantFileCount)
			if params.externalMTLS {
				assert.Equal(t, config.NodeAgentControllerCAPath, files[0].Path)
				assert.Equal(t, "root:root", files[0].Owner)
				assert.Equal(t, "0644", files[0].Permissions)
				assert.Equal(t, "b64", files[0].Encoding)
				assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("public-ca")), files[0].Content)
			}
			agentFile := files[len(files)-1]
			assert.Equal(t, config.NodeAgentConfigPath, agentFile.Path)
			assert.Equal(t, "root:root", agentFile.Owner)
			assert.Equal(t, "0644", agentFile.Permissions)
			assert.Empty(t, agentFile.Encoding)
			assert.Equal(t, params.wantArgs, agentFile.Content)
		})
	}
}
