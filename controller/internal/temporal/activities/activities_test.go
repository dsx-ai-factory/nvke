// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestUnit_NodeAgentBootstrapFiles(t *testing.T) {
	t.Parallel()

	type parameters struct {
		tlsJWT       bool
		externalMTLS bool
		wantArgs     string
	}
	tests := map[string]parameters{
		"SPIFFE without JWT": {
			wantArgs: "AGENT_ARGS=\"--secure-server-port 15005\"\n",
		},
		"SPIFFE with JWT": {
			tlsJWT:   true,
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

// fixedVerdictVerifier is the smallest shared.ImageVerifier: every image gets
// the same answer.
type fixedVerdictVerifier struct{ err error }

func (f fixedVerdictVerifier) VerifyImage(context.Context, *enginev1.MachineImage) error {
	return f.err
}

func TestUnit_ActivitiesRegisterWithATemporalWorker(t *testing.T) {
	t.Parallel()

	// worker.RegisterActivity reflects over every exported method of
	// *Activities and panics on any whose signature is not an activity's. A
	// chainable exported setter once crashed the worker at startup; no unit
	// test caught it because none registered the struct. The test environment
	// runs the same validation as the real worker.
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	require.NotPanics(t, func() {
		env.RegisterActivity(NewActivities(config.Config{}, nil, nil, nil, "", nil, ""))
	})
}

func TestUnit_WithImageVerifierSetsThePreflightSource(t *testing.T) {
	t.Parallel()

	verifier := fixedVerdictVerifier{}
	a := NewActivities(config.Config{}, nil, nil, nil, "", nil, "", WithImageVerifier(verifier))
	assert.Equal(t, verifier, a.imageVerifier)
}
