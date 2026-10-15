// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/utils"
)

func testNodeAgentBootstrapFiles(cfg config.Config, controllerClientCABundle string) func(db.Node) ([]db.CloudInitFile, error) {
	return func(node db.Node) ([]db.CloudInitFile, error) {
		var files []db.CloudInitFile
		args := []string{"--secure-server-port", cfg.NodeAgent.Port}
		if node.NodeAgentExternalMTLSEnabled {
			if controllerClientCABundle == "" {
				return nil, errors.New("controller client CA bundle is required when external node-agent mTLS is enabled")
			}
			files = append(files, db.CloudInitFile{
				Path:        config.NodeAgentControllerCAPath,
				Owner:       "root:root",
				Permissions: "0644",
				Encoding:    "b64",
				Content:     base64.StdEncoding.EncodeToString([]byte(controllerClientCABundle)),
			})
			args = append(args, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)
		}
		if node.NodeAgentTLSJWTEnabled {
			args = append(args, "--grpc-auth-mode=tls-jwt", "--dsx-identity-exchange-url="+cfg.NodeAgent.TLSJWT.DSXIdentityExchangeURL)
		}
		files = append(files, db.CloudInitFile{
			Path:        config.NodeAgentConfigPath,
			Owner:       "root:root",
			Permissions: "0644",
			Content:     fmt.Sprintf("AGENT_ARGS=%q\n", strings.Join(args, " ")),
		})
		return files, nil
	}
}

func testDeps(cfg config.Config, controllerClientCABundle string, bundleLookupCalled *bool, trustDomain, trustBundle string, trustBundleErr error) shared.MachineCreateDeps {
	return shared.MachineCreateDeps{
		Config:                   cfg,
		ControllerClientCABundle: controllerClientCABundle,
		NodeAgentBootstrapFiles:  testNodeAgentBootstrapFiles(cfg, controllerClientCABundle),
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			if bundleLookupCalled != nil {
				*bundleLookupCalled = true
			}
			return trustDomain, trustBundle, trustBundleErr
		},
	}
}

func TestUnit_RenderCAPIBootstrapUserData(t *testing.T) {
	t.Parallel()

	const checksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	customBootstrap := db.CAPINodePoolProvider{
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: "root:root", Permissions: "0600", Content: "hello"},
		},
		PreKubeadmCommands: []string{"echo before kubeadm"},
	}
	tpmSpiffeConfig := config.Config{
		SpiffeEnabled:      true,
		SpireServerAddress: "spire.example.org",
		SpireServerPort:    "9091",
		NodeAgent: config.NodeAgentConfig{
			Port:                   "15005",
			SpireAttestationMethod: config.SpireAttestationMethodTPM,
		},
	}
	joinTokenSpiffeConfig := tpmSpiffeConfig
	joinTokenSpiffeConfig.NodeAgent.SpireAttestationMethod = config.SpireAttestationMethodJoinToken
	dsxIMDSSpiffeConfig := tpmSpiffeConfig
	dsxIMDSSpiffeConfig.NodeAgent.SpireAttestationMethod = config.SpireAttestationMethodDSXIMDS
	dsxIMDSSpiffeConfig.NodeAgent.DSXImds.Endpoint = "http://192.0.2.10"
	dsxIMDSChecksumOverrideConfig := dsxIMDSSpiffeConfig
	dsxIMDSChecksumOverrideConfig.NodeAgent.DSXImds.PluginChecksum = checksum

	type parameters struct {
		config                config.Config
		node                  *db.Node
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
		"existing TPM node ignores new deployment security modes": {
			config: func() config.Config {
				cfg := dsxIMDSSpiffeConfig
				cfg.NodeAgentExternalMTLSEnabled = true
				cfg.NodeAgent.TLSJWT.Enabled = true
				return cfg
			}(),
			node: func() *db.Node {
				node := nodeForBootstrapConfig(tpmSpiffeConfig)
				return &node
			}(),
			trustDomain:           "example.org",
			trustBundle:           "bundle",
			expectTrustBundleCall: true,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, `NodeAttestor "tpm"`)
				assert.NotContains(t, userData, `NodeAttestor "dsx_imds"`)
				assert.NotContains(t, userData, "--grpc-auth-mode=tls-jwt")
				assert.NotContains(t, userData, "--controller-client-ca-bundle")
			},
		},
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
				assert.NotContains(t, userData, `NodeAttestor "dsx_imds"`)
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
				assert.NotContains(t, userData, `NodeAttestor "dsx_imds"`)
			},
		},
		"join-token attestor requires a token": {
			config: config.Config{
				NodeAgent: config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodJoinToken},
			},
			wantErr: "spire join token is required when SPIRE attestation method is joinToken",
		},
		"DSX IMDS attestor uses the node-image checksum by default": {
			config:                dsxIMDSSpiffeConfig,
			trustDomain:           "example.org",
			trustBundle:           "bundle",
			expectTrustBundleCall: true,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assertDSXIMDSAgentConfig(t, userData, "${DSX_IMDS_PLUGIN_CHECKSUM}")
			},
		},
		"DSX IMDS attestor uses the configured checksum override": {
			config:                dsxIMDSChecksumOverrideConfig,
			trustDomain:           "example.org",
			trustBundle:           "bundle",
			expectTrustBundleCall: true,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assertDSXIMDSAgentConfig(t, userData, checksum)
			},
		},
		"SPIFFE disabled does not fetch or render SPIRE data": {
			config:           config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}},
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
			config: config.Config{NodeAgent: config.NodeAgentConfig{
				Port: "15005",
				TLSJWT: config.TLSJWTConfig{
					Enabled:                true,
					DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange",
				},
			}},
			provider: customBootstrap,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered cloudInitConfig
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
			config: config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005", TLSJWT: config.TLSJWTConfig{Enabled: true, DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange"}}},
			provider: db.CAPINodePoolProvider{
				PreKubeadmCommands: []string{"echo prepare agent", "systemctl restart nke-agent"},
			},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered cloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"echo prepare agent", "systemctl restart nke-agent", "systemctl restart nke-agent.service"}, rendered.RunCmd)
			},
		},
		"empty agent version restarts the old unit": {
			config: config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}},
			node:   nodeWithAgentVersion(config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}}, ""),
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered cloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"systemctl restart nke-agent.service"}, rendered.RunCmd)
			},
		},
		"agent version below the cutover restarts the old unit": {
			config: config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}},
			node:   nodeWithAgentVersion(config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}}, "v0.2.0"),
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered cloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"systemctl restart nke-agent.service"}, rendered.RunCmd)
			},
		},
		"agent version at the cutover restarts the new unit": {
			config: config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}},
			node:   nodeWithAgentVersion(config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}}, "v0.3.0"),
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered cloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"systemctl restart nvke-agent.service"}, rendered.RunCmd)
			},
		},
		"agent restarts without caller commands": {
			config: config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				var rendered cloudInitConfig
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				assert.Equal(t, []string{"systemctl restart nke-agent.service"}, rendered.RunCmd)
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			trustBundleCalled := false
			deps := testDeps(params.config, "", &trustBundleCalled, params.trustDomain, params.trustBundle, params.trustBundleError)
			var nodeSet db.NodeSet
			nodeSet.SetCapi(params.provider)
			node := nodeForBootstrapConfig(params.config)
			if params.node != nil {
				node = *params.node
			}

			userData, err := (&Client{}).renderBootstrapUserData(
				context.Background(),
				deps,
				node,
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
	cfg := config.Config{
		NodeAgentExternalMTLSEnabled: true,
		NodeAgent: config.NodeAgentConfig{
			Port:                   "15005",
			SpireAttestationMethod: config.SpireAttestationMethodTPM,
		},
		SpiffeEnabled: true,
	}
	deps := testDeps(cfg, controllerCA, nil, "example.org", "spire trust bundle", nil)

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/provider.conf", Owner: "provider:provider", Permissions: "0600", Content: "provider content"},
		},
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
	})
	userData, err := (&Client{}).renderBootstrapUserData(context.Background(), deps, nodeForBootstrapConfig(cfg), nodeSet, "")
	require.NoError(t, err)

	var cloudConfig cloudInitConfig
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
	cfg := config.Config{NodeAgentExternalMTLSEnabled: true}
	deps := testDeps(cfg, "", nil, "", "", nil)

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{SpireAttestationMethod: db.SpireAttestationMethodTPM})
	_, err := (&Client{}).renderBootstrapUserData(context.Background(), deps, nodeForBootstrapConfig(cfg), nodeSet, "")
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
			cfg := config.Config{
				NodeAgent:                    config.NodeAgentConfig{Port: "15005"},
				NodeAgentExternalMTLSEnabled: params.externalMTLS,
			}
			deps := testDeps(cfg, "public-ca", nil, "", "", nil)
			var nodeSet db.NodeSet
			nodeSet.SetCapi(db.CAPINodePoolProvider{
				BootstrapFiles: []db.CloudInitFile{{Path: params.path, Content: "caller content"}},
			})
			userData, err := (&Client{}).renderBootstrapUserData(context.Background(), deps, nodeForBootstrapConfig(cfg), nodeSet, "")
			require.NoError(t, err)
			var rendered cloudInitConfig
			require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
			require.GreaterOrEqual(t, len(rendered.WriteFiles), 2)
			assert.Equal(t, params.path, rendered.WriteFiles[0].Path)
			assert.Equal(t, "caller content", rendered.WriteFiles[0].Content)
			assert.Equal(t, config.NodeAgentConfigPath, rendered.WriteFiles[len(rendered.WriteFiles)-1].Path)
		})
	}
}

func nodeForBootstrapConfig(cfg config.Config) db.Node {
	method := db.SpireAttestationMethod(cfg.NodeAgent.SpireAttestationMethod)
	if method == "" {
		method = db.SpireAttestationMethodTPM
	}
	return db.Node{
		SpiffeEnabled:                cfg.SpiffeEnabled,
		SpireAttestationMethod:       method,
		NodeAgentExternalMTLSEnabled: cfg.NodeAgentExternalMTLSEnabled,
		NodeAgentTLSJWTEnabled:       cfg.NodeAgent.TLSJWT.Enabled,
	}
}

func assertDSXIMDSAgentConfig(t *testing.T, userData, checksum string) {
	t.Helper()

	assert.Contains(t, userData, `NodeAttestor "dsx_imds"`)
	assert.Contains(t, userData, `plugin_cmd = "/usr/local/lib/spire-agent/dsx-imds-node-attestor-agent"`)
	assert.Contains(t, userData, `plugin_checksum = "`+checksum+`"`)
	assert.Contains(t, userData, `endpoint = "http://192.0.2.10"`)
	assert.Contains(t, userData, `audience = "spiffe://example.org/spire/server"`)
	assert.NotContains(t, userData, `NodeAttestor "join_token"`)
	assert.NotContains(t, userData, `NodeAttestor "tpm"`)
	assert.NotContains(t, userData, "join_token =")
}

func TestUnit_RenderGenericCloudInitFilesTemplate(t *testing.T) {
	t.Parallel()

	const fileTemplate = `write_files:
{{- range .Files }}
  - path: {{ quote .Path }}
    owner: {{ quote .Owner }}
    permissions: {{ quote .Permissions }}
{{- if .Encoding }}
    encoding: {{ quote .Encoding }}
{{- end }}
    content: {{ quote .Content }}
{{- end }}
`
	tmpl := template.Must(template.New("files").Funcs(template.FuncMap{
		"quote": utils.Quote,
	}).Parse(fileTemplate))

	type parameters struct {
		files []db.CloudInitFile
	}
	tests := map[string]parameters{
		"multiple files with YAML-sensitive metadata": {
			files: []db.CloudInitFile{
				{Path: "/etc/example #1", Owner: "root:root", Permissions: "0600", Encoding: "b64", Content: base64.StdEncoding.EncodeToString([]byte("first\nfile\n"))},
				{Path: "/etc/example:2", Owner: "user:group", Permissions: "0644", Content: "second\x00file"},
			},
		},
		"empty files": {},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var rendered bytes.Buffer
			require.NoError(t, tmpl.Execute(&rendered, struct{ Files []db.CloudInitFile }{Files: params.files}))
			assert.Equal(t, 1, strings.Count(rendered.String(), "write_files:"))

			var parsedCloudConfig struct {
				WriteFiles []cloudInitFile `yaml:"write_files"`
			}
			require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &parsedCloudConfig))
			require.Len(t, parsedCloudConfig.WriteFiles, len(params.files))
			for index, want := range params.files {
				got := parsedCloudConfig.WriteFiles[index]
				assert.Equal(t, want.Path, got.Path)
				assert.Equal(t, want.Owner, got.Owner)
				assert.Equal(t, want.Permissions, got.Permissions)
				assert.Equal(t, want.Encoding, got.Encoding)
				assert.Equal(t, want.Content, got.Content)
			}
		})
	}
}

func nodeWithAgentVersion(cfg config.Config, agentVersion string) *db.Node {
	node := nodeForBootstrapConfig(cfg)
	node.AgentVersion = agentVersion
	return &node
}
