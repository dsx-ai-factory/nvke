// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/base64"
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
		NodeAgentPort:                   "15005",
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
				assert.Contains(t, userData, config.NodeAgentConfigPath)
				assert.Contains(t, userData, `AGENT_ARGS="--secure-server-port 15005"`)
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
				assert.Contains(t, userData, config.NodeAgentConfigPath)
				assert.Contains(t, userData, `AGENT_ARGS="--secure-server-port 15005"`)
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
			config:           config.Config{NodeAgentPort: "15005"},
			provider:         customBootstrap,
			trustBundleError: errors.New("workload API is not reachable without SPIRE"),
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.NotContains(t, userData, "/etc/spire-agent/agent.conf")
				assert.NotContains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
				assert.Contains(t, userData, config.NodeAgentConfigPath)
				assert.Contains(t, userData, `AGENT_ARGS="--secure-server-port 15005"`)
				assert.Contains(t, userData, "/etc/custom.conf")
				assert.Contains(t, userData, "echo before kubeadm")
			},
		},
		"TLS JWT renders the agent arguments": {
			config:   config.Config{NodeAgentTLSJWTEnabled: true, NodeAgentPort: "15005", NodeAgentTLSJWTDSXIdentityExchangeURL: "https://identity.example.test/v1/exchange"},
			provider: customBootstrap,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered capiCloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				var agentFiles int
				for _, file := range rendered.WriteFiles {
					if file.Path == config.NodeAgentConfigPath {
						agentFiles++
						assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005 --grpc-auth-mode=tls-jwt --dsx-identity-exchange-url=https://identity.example.test/v1/exchange\"\n", file.Content)
					}
				}
				assert.Equal(t, 1, agentFiles)
				assert.Equal(t, []string{"echo before kubeadm", "systemctl restart nke-agent.service"}, rendered.RunCmd)
				assert.NotContains(t, userData, "sed -E")
			},
		},
		"existing agent restart is followed by the final restart": {
			config: config.Config{NodeAgentTLSJWTEnabled: true, NodeAgentPort: "15005"},
			provider: db.CAPINodePoolProvider{
				PreKubeadmCommands: []string{"echo prepare agent", "systemctl restart nke-agent"},
			},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered capiCloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"echo prepare agent", "systemctl restart nke-agent", "systemctl restart nke-agent.service"}, rendered.RunCmd)
			},
		},
		"agent restarts without caller commands": {
			config: config.Config{NodeAgentPort: "15005"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered capiCloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"systemctl restart nke-agent.service"}, rendered.RunCmd)
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

func TestRenderCAPIBootstrapUserData_ExternalMTLS(t *testing.T) {
	controllerCA := "-----BEGIN CERTIFICATE-----\npublic-ca\n-----END CERTIFICATE-----\n"
	activities := &Activities{
		cfg: config.Config{
			NodeAgentExternalMTLSEnabled:    true,
			NodeAgentPort:                   "15005",
			NodeAgentSpireAttestationMethod: config.SpireAttestationMethodTPM,
			SpiffeEnabled:                   true,
		},
		controllerClientCABundle: controllerCA,
		getTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return "example.org", "spire trust bundle", nil
		},
	}

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/provider.conf", Owner: "provider:provider", Permissions: "0600", Content: "provider content"},
		},
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
	})
	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
	require.NoError(t, err)

	var cloudConfig capiCloudInitConfig
	require.NoError(t, yaml.Unmarshal([]byte(userData), &cloudConfig))
	require.Len(t, cloudConfig.WriteFiles, 5)
	assert.Equal(t, "/etc/provider.conf", cloudConfig.WriteFiles[0].Path)
	assert.Empty(t, cloudConfig.WriteFiles[0].Encoding)
	assert.Equal(t, "provider content", cloudConfig.WriteFiles[0].Content)
	assert.Equal(t, "/etc/spire-agent/initial-trust-bundle.pem", cloudConfig.WriteFiles[1].Path)
	assert.Empty(t, cloudConfig.WriteFiles[1].Encoding)
	assert.Equal(t, "/etc/spire-agent/agent.conf", cloudConfig.WriteFiles[2].Path)
	assert.Empty(t, cloudConfig.WriteFiles[2].Encoding)
	controllerCAFile := cloudConfig.WriteFiles[3]
	assert.Equal(t, config.NodeAgentControllerCAPath, controllerCAFile.Path)
	assert.Equal(t, "root:root", controllerCAFile.Owner)
	assert.Equal(t, "0644", controllerCAFile.Permissions)
	assert.Equal(t, "b64", controllerCAFile.Encoding)
	decodedCA, decodeErr := base64.StdEncoding.DecodeString(controllerCAFile.Content)
	require.NoError(t, decodeErr)
	assert.Equal(t, controllerCA, string(decodedCA))
	agentConfigFile := cloudConfig.WriteFiles[4]
	assert.Equal(t, config.NodeAgentConfigPath, agentConfigFile.Path)
	assert.Equal(t, "root:root", agentConfigFile.Owner)
	assert.Equal(t, "0644", agentConfigFile.Permissions)
	assert.Empty(t, agentConfigFile.Encoding)
	assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005 --controller-client-ca-bundle=/etc/nke/controller-ca.crt\"\n", agentConfigFile.Content)
	assert.Equal(t, []string{"systemctl restart nke-agent.service"}, cloudConfig.RunCmd)
	assert.NotContains(t, userData, "--grpc-auth-mode")
	assert.NotContains(t, userData, "PRIVATE KEY")
}

func TestRenderCAPIBootstrapUserData_ExternalMTLSRequiresCA(t *testing.T) {
	activities := &Activities{
		cfg: config.Config{NodeAgentExternalMTLSEnabled: true},
	}

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{SpireAttestationMethod: db.SpireAttestationMethodTPM})
	_, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
	require.ErrorContains(t, err, "controller client CA bundle is required")
}

func TestUnit_RenderCAPIBootstrapUserData_AllowsFileOverride(t *testing.T) {
	t.Parallel()

	type parameters struct {
		path         string
		externalMTLS bool
	}
	tests := map[string]parameters{
		"agent environment":       {path: config.NodeAgentConfigPath},
		"agent environment alias": {path: "/etc/default/../default/nke-agent"},
		"active controller CA":    {path: config.NodeAgentControllerCAPath, externalMTLS: true},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			activities := &Activities{
				cfg: config.Config{
					NodeAgentPort:                "15005",
					NodeAgentExternalMTLSEnabled: params.externalMTLS,
				},
				controllerClientCABundle: "public-ca",
			}
			var nodeSet db.NodeSet
			nodeSet.SetCapi(db.CAPINodePoolProvider{
				BootstrapFiles: []db.CloudInitFile{{Path: params.path, Content: "caller content"}},
			})
			userData, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
			require.NoError(t, err)
			var rendered capiCloudInitConfig
			require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
			require.GreaterOrEqual(t, len(rendered.WriteFiles), 2)
			assert.Equal(t, params.path, rendered.WriteFiles[0].Path)
			assert.Equal(t, "caller content", rendered.WriteFiles[0].Content)
			assert.Equal(t, config.NodeAgentConfigPath, rendered.WriteFiles[len(rendered.WriteFiles)-1].Path)
		})
	}
}
