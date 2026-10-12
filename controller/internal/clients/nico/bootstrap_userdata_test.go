// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const testControllerClientCA = "-----BEGIN CERTIFICATE-----\npublic-ca\n-----END CERTIFICATE-----\n"

// testNodeAgentBootstrapFiles mirrors Activities.nodeAgentBootstrapFiles for
// tests exercising renderUserData directly, without pulling in the
// activities package.
func testNodeAgentBootstrapFiles(cfg config.Config, controllerClientCABundle string) func(db.Node) ([]db.CloudInitFile, error) {
	return func(node db.Node) ([]db.CloudInitFile, error) {
		var files []db.CloudInitFile
		args := []string{"--secure-server-port", cfg.NodeAgent.Port}
		if node.NodeAgentExternalMTLSEnabled {
			if controllerClientCABundle == "" {
				return nil, fmt.Errorf("controller client CA bundle is required when external node-agent mTLS is enabled")
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

const userDataTemplate = `
username={{ .Username }}
password={{ .HashedPassword }}
hostname={{ .Hostname }}
kubernetesVersion={{ trimPrefix .KubernetesVersion "v" }}
nodeAgentPort={{ .NodeAgentPort }}
diskSelectionStrategy={{ .DiskSelectionStrategy }}
calicoEnabled={{ .CalicoEnabled }}
ovnEnabled={{ .OvnEnabled }}
cniImagePackage={{ .CNIImagePackage }}
cniChartsPackage={{ .CNIChartsPackage }}
files={{ len .Files }}
spiffeEnabled={{ .SpiffeEnabled }}
spireServer={{ .SpireServerAddress }}:{{ .SpireServerPort }}
trustDomain={{ .TrustDomain }}
trustBundle:
{{ indent 2 .TrustBundle }}
teleportEnabled={{ .TeleportEnabled }}
teleportToken={{ .TeleportTokenName }}
teleportProxy={{ .TeleportProxyAddress }}
teleportJoinMethod={{ .TeleportJoinMethod }}
teleportLabels={{ .TeleportLabels }}
`

func TestUnit_RenderUserData(t *testing.T) {
	t.Parallel()

	baseConfig := config.Config{
		DgxcTenantUser:     "test-admin",
		DgxcTenantPassword: "test-password",
		NodeAgent:          config.NodeAgentConfig{Port: "15005"},
		SpireServerAddress: "spire.example.com",
		SpireServerPort:    "9091",
	}
	tlsJWTConfig := baseConfig
	tlsJWTConfig.NodeAgent.TLSJWT.Enabled = true
	tlsJWTConfig.NodeAgent.TLSJWT.DSXIdentityExchangeURL = "https://identity.example.test/v1/exchange"
	externalMTLSConfig := baseConfig
	externalMTLSConfig.NodeAgentExternalMTLSEnabled = true
	combinedConfig := externalMTLSConfig
	combinedConfig.NodeAgent.TLSJWT.Enabled = true
	combinedConfig.NodeAgent.TLSJWT.DSXIdentityExchangeURL = "https://identity.example.test/v1/exchange"

	type parameters struct {
		config                config.Config
		template              string
		node                  db.Node
		diskSelectionStrategy db.DiskSelectionStrategy
		kubernetesVersion     string
		teleport              *TeleportUserDataConfig
		cluster               db.Cluster
		expectBundleLookup    bool
		emptyControllerCA     bool
		wantErr               string
		verify                func(*testing.T, string)
	}
	tests := map[string]parameters{
		"renders profile fields": {
			config:                baseConfig,
			template:              userDataTemplate,
			node:                  db.Node{Name: "test-node", SpiffeEnabled: true},
			diskSelectionStrategy: db.DiskSelectionStrategyLargest,
			kubernetesVersion:     "v1.33.0",
			teleport: &TeleportUserDataConfig{
				Enabled:      true,
				TokenName:    "test-token",
				ProxyAddress: "teleport.example.com:443",
				JoinMethod:   "tpm",
				Labels:       "environment: test",
			},
			cluster:            db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			expectBundleLookup: true,
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, "username=test-admin\n")
				assert.Regexp(t, `(?m)^password=\$6\$[^\n]+$`, userData)
				assert.NotContains(t, userData, "password=test-password")
				assert.Contains(t, userData, "hostname=test-node\n")
				assert.Contains(t, userData, "kubernetesVersion=1.33.0\n")
				assert.Contains(t, userData, "nodeAgentPort=15005\n")
				assert.Contains(t, userData, "diskSelectionStrategy=largest\n")
				assert.Contains(t, userData, "calicoEnabled=true\n")
				assert.Contains(t, userData, "ovnEnabled=false\n")
				assert.Contains(t, userData, "cniImagePackage=nvke-cni-calico-images-3.31.5\n")
				assert.Contains(t, userData, "cniChartsPackage=nvke-cni-calico-manifests-3.31.5\n")
				assert.Contains(t, userData, "files=1\n")
				assert.Contains(t, userData, "spiffeEnabled=true\n")
				assert.Contains(t, userData, "spireServer=spire.example.com:9091\n")
				assert.Contains(t, userData, "trustDomain=example.com\n")
				assert.Contains(t, userData, "trustBundle:\n  first certificate\n  second certificate\n")
				assert.Contains(t, userData, "teleportEnabled=true\n")
				assert.Contains(t, userData, "teleportToken=test-token\n")
				assert.Contains(t, userData, "teleportProxy=teleport.example.com:443\n")
				assert.Contains(t, userData, "teleportJoinMethod=tpm\n")
				assert.Contains(t, userData, "teleportLabels=environment: test\n")
			},
		},
		"renders Calico package names": {
			config:            baseConfig,
			template:          "images={{ .CNIImagePackage }}\ncharts={{ .CNIChartsPackage }}\n",
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Equal(t, "images=nvke-cni-calico-images-3.31.5\ncharts=nvke-cni-calico-manifests-3.31.5\n", userData)
			},
		},
		"renders OVN package names": {
			config:            baseConfig,
			template:          "images={{ .CNIImagePackage }}\ncharts={{ .CNIChartsPackage }}\n",
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNIOvnKubernetes, CniVersion: "1.4.1"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Equal(t, "images=nvke-cni-ovn-images-1.4.1\ncharts=nvke-cni-ovn-charts-1.4.1\n", userData)
			},
		},
		"renders TLS JWT agent file in Nico user data": {
			config: tlsJWTConfig,
			template: `#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: {{ .Hostname }}
  user-data:
    write_files:
{{- range .Files }}
      - path: {{ quote .Path }}
        owner: {{ quote .Owner }}
        permissions: {{ quote .Permissions }}
        content: {{ quote .Content }}
{{- end }}
    runcmd:
      - systemctl restart nke-agent
`,
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, "autoinstall:")
				assert.Contains(t, userData, "hostname: test-node")
				rendered := parseAutoinstallWriteFiles(t, userData)
				require.Len(t, rendered, 1)
				assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005 --grpc-auth-mode=tls-jwt --dsx-identity-exchange-url=https://identity.example.test/v1/exchange\"\n", rendered[0].Content)
				assert.NotContains(t, userData, "sed -E")
			},
		},
		"allows Nico profile without TLS JWT agent file": {
			config:            tlsJWTConfig,
			template:          "#cloud-config\nwrite_files: []\n",
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Equal(t, "#cloud-config\nwrite_files: []\n", userData)
			},
		},
		"renders external mTLS CA through generic files": {
			config: externalMTLSConfig,
			template: `#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: {{ .Hostname }}
  user-data:
    write_files:
{{- range .Files }}
      - path: {{ quote .Path }}
        owner: {{ quote .Owner }}
        permissions: {{ quote .Permissions }}
{{- if .Encoding }}
        encoding: {{ quote .Encoding }}
{{- end }}
        content: {{ quote .Content }}
{{- end }}
`,
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, "autoinstall:")
				assert.Contains(t, userData, "hostname: test-node")

				rendered := parseAutoinstallWriteFiles(t, userData)
				require.Len(t, rendered, 2)
				controllerCAFile := rendered[0]
				assert.Equal(t, config.NodeAgentControllerCAPath, controllerCAFile.Path)
				assert.Equal(t, "root:root", controllerCAFile.Owner)
				assert.Equal(t, "0644", controllerCAFile.Permissions)
				assert.Equal(t, "b64", controllerCAFile.Encoding)
				decodedCA, err := base64.StdEncoding.DecodeString(controllerCAFile.Content)
				require.NoError(t, err)
				assert.Equal(t, testControllerClientCA, string(decodedCA))
				agentConfigFile := rendered[1]
				assert.Equal(t, config.NodeAgentConfigPath, agentConfigFile.Path)
				assert.Empty(t, agentConfigFile.Encoding)
				assert.Contains(t, agentConfigFile.Content, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)
				assert.NotContains(t, userData, "systemctl restart nke-agent.service")
			},
		},
		"combines TLS JWT and external mTLS bootstrap": {
			config: combinedConfig,
			template: `#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: {{ .Hostname }}
  user-data:
    write_files:
{{- range .Files }}
      - path: {{ quote .Path }}
{{- if .Encoding }}
        encoding: {{ quote .Encoding }}
{{- end }}
        content: {{ quote .Content }}
{{- end }}
`,
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			verify: func(t *testing.T, userData string) {
				t.Helper()
				assert.Contains(t, userData, "--grpc-auth-mode=tls-jwt")
				assert.Contains(t, userData, "--dsx-identity-exchange-url=https://identity.example.test/v1/exchange")
				assert.Contains(t, userData, config.NodeAgentControllerCAPath)
				assert.Contains(t, userData, base64.StdEncoding.EncodeToString([]byte(testControllerClientCA)))
				assert.Contains(t, userData, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)
			},
		},
		"external mTLS requires a controller CA": {
			config:            externalMTLSConfig,
			template:          "files={{ len .Files }}\n",
			node:              db.Node{Name: "test-node"},
			kubernetesVersion: "1.33.0",
			cluster:           db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			emptyControllerCA: true,
			wantErr:           "controller client CA bundle is required when external node-agent mTLS is enabled",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			params.node.NodeAgentExternalMTLSEnabled = params.config.NodeAgentExternalMTLSEnabled
			params.node.NodeAgentTLSJWTEnabled = params.config.NodeAgent.TLSJWT.Enabled
			bundleLookupCalled := false
			controllerCA := testControllerClientCA
			if params.emptyControllerCA {
				controllerCA = ""
			}
			deps := shared.MachineCreateDeps{
				Config:                   params.config,
				ControllerClientCABundle: controllerCA,
				NodeAgentBootstrapFiles:  testNodeAgentBootstrapFiles(params.config, controllerCA),
				GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
					bundleLookupCalled = true
					return "example.com", "first certificate\nsecond certificate\n", nil
				},
			}
			userData, err := renderUserData(
				context.Background(),
				deps,
				params.template,
				params.node,
				params.diskSelectionStrategy,
				params.kubernetesVersion,
				params.teleport,
				params.cluster,
			)
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, params.expectBundleLookup, bundleLookupCalled)
			params.verify(t, userData)
		})
	}
}

type testWriteFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Encoding    string `yaml:"encoding"`
	Content     string `yaml:"content"`
}

// parseAutoinstallWriteFiles decodes the write_files list out of a rendered
// "#cloud-config autoinstall" document, the shape Nico OS profile templates
// use.
func parseAutoinstallWriteFiles(t *testing.T, userData string) []testWriteFile {
	t.Helper()
	var cloudConfig struct {
		Autoinstall struct {
			UserData struct {
				WriteFiles []testWriteFile `yaml:"write_files"`
			} `yaml:"user-data"`
		} `yaml:"autoinstall"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(userData), &cloudConfig))
	return cloudConfig.Autoinstall.UserData.WriteFiles
}

func TestUnit_RenderUserData_ProfileFixturesSupportGenericFiles(t *testing.T) {
	t.Parallel()

	releasesDir := filepath.Join("..", "..", "..", "test", "fixtures", "releases")
	fixtureTemplates := map[string]func(t *testing.T) string{}
	for _, name := range []string{
		"nvke-local-dev-kindest-arm64",
		"nvke-nico-vms-24.04-amd64-fde",
		"nvke-nico-vms-24.04-arm64-fde",
		"test-fixture-amd64",
	} {
		fixtureTemplates["release "+name] = func(t *testing.T) string {
			t.Helper()
			return releaseUserDataTemplate(t, filepath.Join(releasesDir, name+"-nodepoolrelease.yaml"))
		}
	}
	fixtureTemplates["nico-sim prod-userdata"] = func(t *testing.T) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "nico-sim", "test", "integration", "testutil", "testdata", "prod-userdata.tmpl"))
		require.NoError(t, err)
		return string(data)
	}

	for fixtureName, load := range fixtureTemplates {
		t.Run(fixtureName, func(t *testing.T) {
			t.Parallel()

			templateBytes := []byte(load(t))
			cfg := config.Config{
				DgxcTenantUser:               "test-admin",
				DgxcTenantPassword:           "test-password",
				NodeAgentExternalMTLSEnabled: true,
				NodeAgent: config.NodeAgentConfig{
					Port:                   "15005",
					SpireAttestationMethod: config.SpireAttestationMethodTPM,
				},
			}
			deps := shared.MachineCreateDeps{
				Config:                   cfg,
				ControllerClientCABundle: testControllerClientCA,
				NodeAgentBootstrapFiles:  testNodeAgentBootstrapFiles(cfg, testControllerClientCA),
				GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
					return "example.com", "first certificate\nsecond certificate\n", nil
				},
			}
			userData, err := renderUserData(
				context.Background(),
				deps,
				string(templateBytes),
				db.Node{Name: "test-node", SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true},
				db.DiskSelectionStrategyLargest,
				"1.33.0",
				&TeleportUserDataConfig{},
				db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			)
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(userData, "write_files:"))
			assert.Equal(t, 1, strings.Count(userData, "path: \""+config.NodeAgentConfigPath+"\""))
			assert.NotContains(t, userData, "printf 'AGENT_ARGS=")
			assert.Contains(t, userData, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)

			writeFiles := parseWriteFiles(t, userData)
			var controllerCAFile *testWriteFile
			var agentConfigFile *testWriteFile
			for index := range writeFiles {
				if writeFiles[index].Path == config.NodeAgentControllerCAPath {
					controllerCAFile = &writeFiles[index]
				}
				if writeFiles[index].Path == config.NodeAgentConfigPath {
					agentConfigFile = &writeFiles[index]
				}
			}
			require.NotNil(t, controllerCAFile)
			assert.Equal(t, "root:root", controllerCAFile.Owner)
			assert.Equal(t, "0644", controllerCAFile.Permissions)
			assert.Equal(t, "b64", controllerCAFile.Encoding)
			decodedCA, err := base64.StdEncoding.DecodeString(controllerCAFile.Content)
			require.NoError(t, err)
			assert.Equal(t, testControllerClientCA, string(decodedCA))
			require.NotNil(t, agentConfigFile)
			assert.Equal(t, "root:root", agentConfigFile.Owner)
			assert.Equal(t, "0644", agentConfigFile.Permissions)
			assert.Empty(t, agentConfigFile.Encoding)
			assert.Contains(t, agentConfigFile.Content, "--secure-server-port 15005")
			assert.Contains(t, agentConfigFile.Content, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)

			cfgNoExternalMTLS := cfg
			cfgNoExternalMTLS.NodeAgentExternalMTLSEnabled = false
			deps.Config = cfgNoExternalMTLS
			deps.ControllerClientCABundle = ""
			deps.NodeAgentBootstrapFiles = testNodeAgentBootstrapFiles(cfgNoExternalMTLS, "")
			userData, err = renderUserData(
				context.Background(),
				deps,
				string(templateBytes),
				db.Node{Name: "test-node", SpiffeEnabled: true},
				db.DiskSelectionStrategyLargest,
				"1.33.0",
				&TeleportUserDataConfig{},
				db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			)
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(userData, "write_files:"))
			assert.Equal(t, 1, strings.Count(userData, "path: \""+config.NodeAgentConfigPath+"\""))
			assert.NotContains(t, userData, "printf 'AGENT_ARGS=")
			assert.NotContains(t, userData, "--controller-client-ca-bundle")
			var defaultAgentConfigFile *testWriteFile
			writeFiles = parseWriteFiles(t, userData)
			for index := range writeFiles {
				assert.NotEqual(t, config.NodeAgentControllerCAPath, writeFiles[index].Path)
				if writeFiles[index].Path == config.NodeAgentConfigPath {
					defaultAgentConfigFile = &writeFiles[index]
					assert.NotContains(t, writeFiles[index].Content, "--controller-client-ca-bundle")
				}
			}
			require.NotNil(t, defaultAgentConfigFile)
			assert.Equal(t, `AGENT_ARGS="--secure-server-port 15005"`+"\n", defaultAgentConfigFile.Content)
		})
	}
}

// releaseUserDataTemplate reads the first-boot template out of a release
// fixture, so these cases cover the templates the controller actually renders rather
// than a copy of them.
func releaseUserDataTemplate(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var release struct {
		Spec struct {
			MachineInit struct {
				UserDataTemplate string `yaml:"userDataTemplate"`
			} `yaml:"machineInit"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &release))
	require.NotEmpty(t, release.Spec.MachineInit.UserDataTemplate)

	return release.Spec.MachineInit.UserDataTemplate
}

// parseWriteFiles decodes write_files whether the document is a bare
// cloud-config or wraps user-data inside an autoinstall block, mirroring
// what Nico OS profile fixtures actually produce.
func parseWriteFiles(t *testing.T, userData string) []testWriteFile {
	t.Helper()
	var cloudConfig struct {
		WriteFiles  []testWriteFile `yaml:"write_files"`
		Autoinstall struct {
			UserData struct {
				WriteFiles []testWriteFile `yaml:"write_files"`
			} `yaml:"user-data"`
		} `yaml:"autoinstall"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(userData), &cloudConfig))
	if len(cloudConfig.WriteFiles) > 0 {
		return cloudConfig.WriteFiles
	}
	return cloudConfig.Autoinstall.UserData.WriteFiles
}

func TestBuildTeleportLabels(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	siteID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	clusterID := uuid.MustParse("66666666-7777-8888-9999-000000000000")
	nodePoolID := uuid.MustParse("abababab-cdcd-efef-0101-232323232323")

	for _, params := range []struct {
		name         string
		controlPlane bool
		wantRole     string
	}{
		{name: "control plane node", controlPlane: true, wantRole: "control-plane"},
		{name: "worker node", controlPlane: false, wantRole: "worker"},
	} {
		t.Run(params.name, func(t *testing.T) {
			got := buildTeleportLabels(
				db.Node{ID: nodeID, Name: "my-node-01"},
				db.Cluster{ID: clusterID, Name: "test-cluster"},
				db.NicoClusterProvider{SiteID: siteID},
				nodePoolID,
				params.controlPlane,
				"test-site",
				"staging",
				"nke-cloud-workflow-engine",
				"nke",
			)

			assert.Contains(t, got, "node-role: "+params.wantRole+"\n")
			assert.Contains(t, got, "node-name: my-node-01\n")
			assert.Contains(t, got, "node-id: "+nodeID.String()+"\n")
			assert.Contains(t, got, "site: "+siteID.String()+"\n")
			assert.Contains(t, got, "nke-site-controller-name: test-site\n")
			assert.Contains(t, got, "cluster: "+clusterID.String()+"\n")
			assert.Contains(t, got, "cluster-name: test-cluster\n")
			assert.Contains(t, got, "node-pool: "+nodePoolID.String()+"\n")
			assert.Contains(t, got, "environment: staging\n")
			assert.Contains(t, got, "resource-group: nke\n")
			assert.Contains(t, got, "managed-by: nke-cloud-workflow-engine\n")
		})
	}

	t.Run("nil node pool id", func(t *testing.T) {
		got := buildTeleportLabels(
			db.Node{ID: nodeID, Name: "my-node-01"},
			db.Cluster{ID: clusterID, Name: "test-cluster"},
			db.NicoClusterProvider{SiteID: siteID},
			uuid.Nil,
			false,
			"test-site",
			"staging",
			"nke-cloud-workflow-engine",
			"nke",
		)

		assert.NotContains(t, got, "node-pool:")
		assert.NotContains(t, got, uuid.Nil.String())
	})
}
