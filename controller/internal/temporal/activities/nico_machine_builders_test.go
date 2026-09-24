// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/utils"
)

func TestNicoMachineBuilders_ResolveTemplateLessForgeCAPINodeConfig(t *testing.T) {
	controlPlaneInstanceTypeID := uuid.New()
	workerInstanceTypeID := uuid.New()
	controlPlaneSubnetID := uuid.New()
	workerVpcPrefixID := uuid.New()
	workerNodePoolID := uuid.New()
	infinibandPartitionID := uuid.New()
	nvLinkLogicalPartitionID := uuid.New()

	controlPlaneNodePool := db.NodePool{ControlPlane: true}
	controlPlaneNodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: controlPlaneInstanceTypeID,
		SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
		SubnetIDs:      []uuid.UUID{controlPlaneSubnetID},
	})

	workerNodePool := db.NodePool{ID: workerNodePoolID}
	workerNodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID:           workerInstanceTypeID,
		SSHKeyGroupIDs:           []uuid.UUID{uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
		VpcPrefixIDs:             []uuid.UUID{workerVpcPrefixID},
		InfinibandPartitionID:    &infinibandPartitionID,
		NvLinkLogicalPartitionID: &nvLinkLogicalPartitionID,
	})

	type parameters struct {
		createCtx *capiMachineCreateContext
		want      templateLessForgeCAPINodeConfig
	}

	tests := map[string]parameters{
		"control plane uses resolved node context for config": {
			createCtx: &capiMachineCreateContext{
				node: db.Node{ControlPlane: true, Nico: &db.NicoNodeProvider{InstanceTypeID: controlPlaneInstanceTypeID}},
				nodeSet: func() db.NodeSet {
					nodeSet := db.NodeSet{
						KubernetesVersion: "1.32.1",
						TeleportEnabled:   true,
					}
					nodeSet.SetNico(db.NicoNodePoolProvider{
						OSProfile:             "cp-os",
						DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
					})
					return nodeSet
				}(),
				nodePool: controlPlaneNodePool,
				cluster: db.Cluster{
					ControlPlaneConfig: db.ControlPlaneConfig{
						ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
							// Deliberately different from the resolved node context to ensure no fallback drift.
							KubernetesVersion: "1.30.0",
							TeleportEnabled:   false,
							Nico: &db.NicoControlPlaneProvider{
								OSProfile:      "stale-cluster-control-plane-os",
								SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")},
								SubnetIDs:      []uuid.UUID{uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")},
							},
						},
					},
				},
			},
			want: templateLessForgeCAPINodeConfig{
				osProfile:             "cp-os",
				diskSelectionStrategy: db.DiskSelectionStrategySmallest,
				kubernetesVersion:     "1.32.1",
				teleportEnabled:       true,
				instanceTypeID:        controlPlaneInstanceTypeID,
				sshKeyGroupIDs:        []uuid.UUID{uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
				subnetIDs:             []uuid.UUID{controlPlaneSubnetID},
				nodePoolID:            uuid.Nil,
				isControlPlaneRole:    true,
			},
		},
		"worker uses node set and node pool config": {
			createCtx: &capiMachineCreateContext{
				node: db.Node{
					NodePoolID: workerNodePoolID,
				},
				nodeSet: func() db.NodeSet {
					nodeSet := db.NodeSet{
						KubernetesVersion: "1.33.0",
						TeleportEnabled:   true,
					}
					nodeSet.SetNico(db.NicoNodePoolProvider{
						OSProfile:             "worker-os",
						DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
					})
					return nodeSet
				}(),
				nodePool: workerNodePool,
			},
			want: templateLessForgeCAPINodeConfig{
				osProfile:                "worker-os",
				diskSelectionStrategy:    db.DiskSelectionStrategyLargest,
				kubernetesVersion:        "1.33.0",
				teleportEnabled:          true,
				instanceTypeID:           workerInstanceTypeID,
				sshKeyGroupIDs:           []uuid.UUID{uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
				vpcPrefixIDs:             []uuid.UUID{workerVpcPrefixID},
				nodePoolID:               workerNodePoolID,
				infinibandPartitionID:    &infinibandPartitionID,
				nvLinkLogicalPartitionID: &nvLinkLogicalPartitionID,
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := resolveTemplateLessForgeCAPINodeConfig(params.createCtx)
			require.NoError(t, err)
			assert.Equal(t, params.want, got)
		})
	}
}

func TestNicoMachineBuilders_ControlPlaneConfigPinning(t *testing.T) {
	controlPlaneInstanceTypeID := uuid.New()
	pinnedSubnetID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	pinnedSSHKeyGroupID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")

	// Synthetic no-nodepool control-plane context from nodes.GetNodeContext.
	nodePool := db.NodePool{ID: uuid.Nil, ControlPlane: true}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: controlPlaneInstanceTypeID,
		SSHKeyGroupIDs: []uuid.UUID{pinnedSSHKeyGroupID},
		SubnetIDs:      []uuid.UUID{pinnedSubnetID},
	})

	nodeSet := db.NodeSet{
		KubernetesVersion: "1.33.7",
		TeleportEnabled:   true,
	}
	nodeSet.SetNico(db.NicoNodePoolProvider{
		OSProfile:             "cp-os-a",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
	})

	createCtx := &capiMachineCreateContext{
		node: db.Node{
			ControlPlane: true,
			Nico: &db.NicoNodeProvider{
				InstanceTypeID: controlPlaneInstanceTypeID,
			},
		},
		nodeSet:  nodeSet,
		nodePool: nodePool,
		cluster: db.Cluster{
			ControlPlaneConfig: db.ControlPlaneConfig{
				ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
					KubernetesVersion: "1.40.1",
					TeleportEnabled:   false,
					Nico: &db.NicoControlPlaneProvider{
						OSProfile:      "cp-os-b",
						SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")},
						SubnetIDs:      []uuid.UUID{uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001")},
					},
				},
			},
		},
	}

	got, err := resolveTemplateLessForgeCAPINodeConfig(createCtx)
	require.NoError(t, err)
	assert.Equal(t, "cp-os-a", got.osProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, got.diskSelectionStrategy)
	assert.Equal(t, "1.33.7", got.kubernetesVersion)
	assert.True(t, got.teleportEnabled)
	assert.Equal(t, controlPlaneInstanceTypeID, got.instanceTypeID)
	assert.Equal(t, []uuid.UUID{pinnedSSHKeyGroupID}, got.sshKeyGroupIDs)
	assert.Equal(t, []uuid.UUID{pinnedSubnetID}, got.subnetIDs)
	assert.Equal(t, uuid.Nil, got.nodePoolID)
	assert.True(t, got.isControlPlaneRole)
}

func TestNicoMachineBuilders_ValidateTemplateLessForgeCAPINodeConfig(t *testing.T) {
	type parameters struct {
		createCtx      *capiMachineCreateContext
		wantErrContain string
	}

	requiresOSProfilePool := db.NodePool{}
	requiresOSProfilePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: uuid.New(),
		SubnetIDs:      []uuid.UUID{uuid.New()},
	})

	requiresInstanceTypePool := db.NodePool{}
	requiresInstanceTypePool.SetNico(db.NicoNodePoolProvider{
		SubnetIDs: []uuid.UUID{uuid.New()},
	})

	requiresNetworkPool := db.NodePool{}
	requiresNetworkPool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: uuid.New(),
	})

	emptyNicoPool := db.NodePool{}
	emptyNicoPool.SetNico(db.NicoNodePoolProvider{})

	tests := map[string]parameters{
		"fails fast when resolved context is missing": {
			createCtx: &capiMachineCreateContext{
				node: db.Node{Nico: &db.NicoNodeProvider{InstanceTypeID: uuid.New()}},
				nodeSet: func() db.NodeSet {
					nodeSet := db.NodeSet{}
					nodeSet.SetNico(db.NicoNodePoolProvider{})
					return nodeSet
				}(),
				nodePool: emptyNicoPool,
			},
			wantErrContain: "resolved node context is incomplete",
		},
		"requires os profile": {
			createCtx: &capiMachineCreateContext{
				nodeSet: func() db.NodeSet {
					nodeSet := db.NodeSet{KubernetesVersion: "1.33.0"}
					nodeSet.SetNico(db.NicoNodePoolProvider{})
					return nodeSet
				}(),
				nodePool: requiresOSProfilePool,
			},
			wantErrContain: "nodeSet.osProfile",
		},
		"requires instance type": {
			createCtx: &capiMachineCreateContext{
				nodeSet: func() db.NodeSet {
					nodeSet := db.NodeSet{KubernetesVersion: "1.33.0"}
					nodeSet.SetNico(db.NicoNodePoolProvider{OSProfile: "cp-os"})
					return nodeSet
				}(),
				nodePool: requiresInstanceTypePool,
			},
			wantErrContain: "nodePool.instanceTypeID",
		},
		"requires network candidates": {
			createCtx: &capiMachineCreateContext{
				nodeSet: func() db.NodeSet {
					nodeSet := db.NodeSet{KubernetesVersion: "1.33.0"}
					nodeSet.SetNico(db.NicoNodePoolProvider{OSProfile: "cp-os"})
					return nodeSet
				}(),
				nodePool: requiresNetworkPool,
			},
			wantErrContain: "nodePool.subnetIDs|vpcPrefixIDs",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := resolveTemplateLessForgeCAPINodeConfig(params.createCtx)
			require.ErrorContains(t, err, params.wantErrContain)
		})
	}
}

func TestNicoMachineBuilders_BuildNicoMachineInterfaces(t *testing.T) {
	subnetID := uuid.New()
	vpcPrefixID := uuid.New()

	type parameters struct {
		subnetIDs      []uuid.UUID
		vpcPrefixIDs   []uuid.UUID
		wantSubnetID   string
		wantPrefixID   string
		wantErrContain string
	}

	tests := map[string]parameters{
		"prefers vpc prefix over subnet": {
			subnetIDs:    []uuid.UUID{subnetID},
			vpcPrefixIDs: []uuid.UUID{vpcPrefixID},
			wantPrefixID: vpcPrefixID.String(),
		},
		"uses subnet when prefix is absent": {
			subnetIDs:    []uuid.UUID{subnetID},
			wantSubnetID: subnetID.String(),
		},
		"requires a candidate": {
			wantErrContain: "both subnetIDs and vpcPrefixIDs are empty",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			interfaces, err := buildNicoMachineInterfaces(params.subnetIDs, params.vpcPrefixIDs)
			if params.wantErrContain != "" {
				require.ErrorContains(t, err, params.wantErrContain)
				return
			}
			require.NoError(t, err)
			require.Len(t, interfaces, 1)
			assert.Equal(t, params.wantSubnetID, interfaces[0].SubnetID)
			assert.Equal(t, params.wantPrefixID, interfaces[0].VPCPrefixID)
		})
	}
}

func TestNicoMachineBuilders_RenderIpxeScript(t *testing.T) {
	script, err := renderIpxeScript("set disk-strategy {{ .DiskSelectionStrategy }}\n", string(db.DiskSelectionStrategyLargest))
	require.NoError(t, err)
	assert.Equal(t, "set disk-strategy largest\n", script)
}

const testControllerClientCA = "-----BEGIN CERTIFICATE-----\npublic-ca\n-----END CERTIFICATE-----\n"

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
		NodeAgentPort:      "15005",
		SpireServerAddress: "spire.example.com",
		SpireServerPort:    "9091",
	}
	tlsJWTConfig := baseConfig
	tlsJWTConfig.NodeAgentTLSJWTEnabled = true
	tlsJWTConfig.NodeAgentTLSJWTDSXIdentityExchangeURL = "https://identity.example.test/v1/exchange"
	externalMTLSConfig := baseConfig
	externalMTLSConfig.NodeAgentExternalMTLSEnabled = true
	combinedConfig := externalMTLSConfig
	combinedConfig.NodeAgentTLSJWTEnabled = true
	combinedConfig.NodeAgentTLSJWTDSXIdentityExchangeURL = "https://identity.example.test/v1/exchange"

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
				assert.Contains(t, userData, "cniImagePackage=nke-cni-calico-images-3.31.5\n")
				assert.Contains(t, userData, "cniChartsPackage=nke-cni-calico-manifests-3.31.5\n")
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
				assert.Equal(t, "images=nke-cni-calico-images-3.31.5\ncharts=nke-cni-calico-manifests-3.31.5\n", userData)
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
				assert.Equal(t, "images=nke-cni-ovn-images-1.4.1\ncharts=nke-cni-ovn-charts-1.4.1\n", userData)
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
				var rendered struct {
					Autoinstall struct {
						UserData capiCloudInitConfig `yaml:"user-data"`
					} `yaml:"autoinstall"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				require.Len(t, rendered.Autoinstall.UserData.WriteFiles, 1)
				assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005 --grpc-auth-mode=tls-jwt --dsx-identity-exchange-url=https://identity.example.test/v1/exchange\"\n", rendered.Autoinstall.UserData.WriteFiles[0].Content)
				assert.Equal(t, []string{"systemctl restart nke-agent"}, rendered.Autoinstall.UserData.RunCmd)
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

				var rendered struct {
					Autoinstall struct {
						UserData capiCloudInitConfig `yaml:"user-data"`
					} `yaml:"autoinstall"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(userData), &rendered))
				require.Len(t, rendered.Autoinstall.UserData.WriteFiles, 2)
				controllerCAFile := rendered.Autoinstall.UserData.WriteFiles[0]
				assert.Equal(t, config.NodeAgentControllerCAPath, controllerCAFile.Path)
				assert.Equal(t, "root:root", controllerCAFile.Owner)
				assert.Equal(t, "0644", controllerCAFile.Permissions)
				assert.Equal(t, "b64", controllerCAFile.Encoding)
				decodedCA, err := base64.StdEncoding.DecodeString(controllerCAFile.Content)
				require.NoError(t, err)
				assert.Equal(t, testControllerClientCA, string(decodedCA))
				agentConfigFile := rendered.Autoinstall.UserData.WriteFiles[1]
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
			bundleLookupCalled := false
			controllerCA := testControllerClientCA
			if params.emptyControllerCA {
				controllerCA = ""
			}
			activities := &Activities{
				cfg:                      params.config,
				controllerClientCABundle: controllerCA,
				getTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
					bundleLookupCalled = true
					return "example.com", "first certificate\nsecond certificate\n", nil
				},
			}
			userData, err := activities.renderUserData(
				context.Background(),
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

			var cloudConfig struct {
				WriteFiles []capiCloudInitFile `yaml:"write_files"`
			}
			require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &cloudConfig))
			require.Len(t, cloudConfig.WriteFiles, len(params.files))
			for index, want := range params.files {
				got := cloudConfig.WriteFiles[index]
				assert.Equal(t, want.Path, got.Path)
				assert.Equal(t, want.Owner, got.Owner)
				assert.Equal(t, want.Permissions, got.Permissions)
				assert.Equal(t, want.Encoding, got.Encoding)
				assert.Equal(t, want.Content, got.Content)
			}
		})
	}
}

func TestUnit_RenderUserData_ProfileFixturesSupportGenericFiles(t *testing.T) {
	t.Parallel()
	parseWriteFiles := func(t *testing.T, userData string) []capiCloudInitFile {
		t.Helper()
		var cloudConfig struct {
			WriteFiles  []capiCloudInitFile `yaml:"write_files"`
			Autoinstall struct {
				UserData struct {
					WriteFiles []capiCloudInitFile `yaml:"write_files"`
				} `yaml:"user-data"`
			} `yaml:"autoinstall"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(userData), &cloudConfig))
		if len(cloudConfig.WriteFiles) > 0 {
			return cloudConfig.WriteFiles
		}
		return cloudConfig.Autoinstall.UserData.WriteFiles
	}

	fixturePaths := []string{
		filepath.Join("..", "..", "..", "profiles", "example", "profiles", "osProfiles", "example-ubuntu-24.04-amd64", "userData.tmpl"),
		filepath.Join("..", "..", "..", "test", "fixtures", "profiles", "osProfiles", "nke-local-dev-kindest-arm64", "userData.tmpl"),
		filepath.Join("..", "..", "..", "test", "fixtures", "profiles", "osProfiles", "nke-nico-vms-24.04-amd64-fde", "userData.tmpl"),
		filepath.Join("..", "..", "..", "test", "fixtures", "profiles", "osProfiles", "nke-nico-vms-24.04-arm64-fde", "userData.tmpl"),
		filepath.Join("..", "..", "..", "test", "fixtures", "profiles", "osProfiles", "test-fixture-amd64", "userData.tmpl"),
		filepath.Join("..", "..", "..", "nico-sim", "test", "integration", "testutil", "testdata", "prod-userdata.tmpl"),
	}

	for _, fixturePath := range fixturePaths {
		t.Run(fixturePath, func(t *testing.T) {
			t.Parallel()

			templateBytes, err := os.ReadFile(fixturePath)
			require.NoError(t, err)
			activities := &Activities{
				cfg: config.Config{
					DgxcTenantUser:                  "test-admin",
					DgxcTenantPassword:              "test-password",
					NodeAgentExternalMTLSEnabled:    true,
					NodeAgentPort:                   "15005",
					NodeAgentSpireAttestationMethod: config.SpireAttestationMethodTPM,
				},
				controllerClientCABundle: testControllerClientCA,
				getTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
					return "example.com", "first certificate\nsecond certificate\n", nil
				},
			}
			userData, err := activities.renderUserData(
				context.Background(),
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
			assert.Contains(t, userData, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)

			writeFiles := parseWriteFiles(t, userData)
			var controllerCAFile *capiCloudInitFile
			var agentConfigFile *capiCloudInitFile
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

			activities.cfg.NodeAgentExternalMTLSEnabled = false
			activities.controllerClientCABundle = ""
			userData, err = activities.renderUserData(
				context.Background(),
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
			var defaultAgentConfigFile *capiCloudInitFile
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

func TestNicoMachineBuilders_BuildTeleportLabels(t *testing.T) {
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
		)

		assert.NotContains(t, got, "node-pool:")
		assert.NotContains(t, got, uuid.Nil.String())
	})
}
