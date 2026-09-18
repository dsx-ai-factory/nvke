// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestUnit_RenderCAPIBootstrapUserData(t *testing.T) {
	t.Parallel()

	customBootstrap := db.CAPINodePoolProvider{
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: "root:root", Permissions: "0600", Content: "hello"},
		},
		PreKubeadmCommands: []string{"echo before kubeadm"},
	}
	tpmSpiffeConfig := config.Config{
		SpiffeEnabled:                   true,
		SpireServerAddress:              "spire.example.org",
		SpireServerPort:                 "9091",
		NodeAgentSpireAttestationMethod: config.SpireAttestationMethodTPM,
	}
	joinTokenSpiffeConfig := tpmSpiffeConfig
	joinTokenSpiffeConfig.NodeAgentSpireAttestationMethod = config.SpireAttestationMethodJoinToken

	type parameters struct {
		config                config.Config
		provider              db.CAPINodePoolProvider
		spireJoinToken        string
		trustDomain           string
		trustBundle           string
		trustBundleError      error
		expectTrustBundleCall bool
		wantErr               string
		verify                func(*testing.T, string)
	}
	tests := map[string]parameters{
		"TPM attestor": {
			config:                tpmSpiffeConfig,
			provider:              customBootstrap,
			trustDomain:           "example.org",
			trustBundle:           "-----BEGIN CERT-----\nabc123\n-----END CERT-----",
			expectTrustBundleCall: true,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, "#cloud-config\n")
				assert.Contains(t, userData, "/etc/custom.conf")
				assert.Contains(t, userData, "/etc/spire-agent/agent.conf")
				assert.Contains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
				assert.Contains(t, userData, `NodeAttestor "tpm"`)
				assert.NotContains(t, userData, `NodeAttestor "join_token"`)
				assert.Contains(t, userData, "echo before kubeadm")
				var parsed map[string]interface{}
				require.NoError(t, yaml.Unmarshal([]byte(userData), &parsed))
			},
		},
		"join-token attestor": {
			config:                joinTokenSpiffeConfig,
			spireJoinToken:        "join-token-123",
			trustDomain:           "example.org",
			trustBundle:           "bundle",
			expectTrustBundleCall: true,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, `join_token = "join-token-123"`)
				assert.Contains(t, userData, `NodeAttestor "join_token"`)
				assert.NotContains(t, userData, `NodeAttestor "tpm"`)
			},
		},
		"join-token attestor requires a token": {
			config: config.Config{
				NodeAgentSpireAttestationMethod: config.SpireAttestationMethodJoinToken,
			},
			wantErr: "spire join token is required when SPIRE attestation method is joinToken",
		},
		"SPIFFE disabled does not fetch or render SPIRE data": {
			provider:         customBootstrap,
			trustBundleError: errors.New("workload API is not reachable without SPIRE"),
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.NotContains(t, userData, "/etc/spire-agent/agent.conf")
				assert.NotContains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
				assert.Contains(t, userData, "/etc/custom.conf")
				assert.Contains(t, userData, "echo before kubeadm")
			},
		},
		"TLS JWT renders the agent arguments": {
			config: config.Config{NodeAgentTLSJWTEnabled: true},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, "--grpc-auth-mode=tls-jwt")
				assert.Contains(t, userData, "systemctl restart nke-agent.service")
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			trustBundleCalled := false
			activities := &Activities{
				cfg: params.config,
				getTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
					trustBundleCalled = true
					return params.trustDomain, params.trustBundle, params.trustBundleError
				},
			}
			var nodeSet db.NodeSet
			nodeSet.SetCapi(params.provider)

			userData, err := activities.renderCAPIBootstrapUserData(
				context.Background(),
				nodeSet,
				params.spireJoinToken,
			)

			assert.Equal(t, params.expectTrustBundleCall, trustBundleCalled)
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			params.verify(t, userData)
		})
	}
}
