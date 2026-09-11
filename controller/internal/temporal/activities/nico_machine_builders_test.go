// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
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

func TestNicoMachineBuilders_RenderUserData(t *testing.T) {
	bundleLookupCalled := false
	activities := &Activities{
		cfg: config.Config{
			DgxcTenantUser:     "test-admin",
			DgxcTenantPassword: "test-password",
			NodeAgentPort:      "15005",
			SpireServerAddress: "spire.example.com",
			SpireServerPort:    "9091",
		},
		getTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			bundleLookupCalled = true
			return "example.com", "first certificate\nsecond certificate\n", nil
		},
	}

	userData, err := activities.renderUserData(
		context.Background(),
		userDataTemplate,
		db.Node{Name: "test-node", SpiffeEnabled: true},
		db.DiskSelectionStrategyLargest,
		"v1.33.0",
		&TeleportUserDataConfig{
			Enabled:      true,
			TokenName:    "test-token",
			ProxyAddress: "teleport.example.com:443",
			JoinMethod:   "tpm",
			Labels:       "environment: test",
		},
		db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
	)
	require.NoError(t, err)

	assert.True(t, bundleLookupCalled)
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
	assert.Contains(t, userData, "spiffeEnabled=true\n")
	assert.Contains(t, userData, "spireServer=spire.example.com:9091\n")
	assert.Contains(t, userData, "trustDomain=example.com\n")
	assert.Contains(t, userData, "trustBundle:\n  first certificate\n  second certificate\n")
	assert.Contains(t, userData, "teleportEnabled=true\n")
	assert.Contains(t, userData, "teleportToken=test-token\n")
	assert.Contains(t, userData, "teleportProxy=teleport.example.com:443\n")
	assert.Contains(t, userData, "teleportJoinMethod=tpm\n")
	assert.Contains(t, userData, "teleportLabels=environment: test\n")
}

func TestNicoMachineBuilders_RenderUserDataCNIPackages(t *testing.T) {
	const userDataTemplate = "images={{ .CNIImagePackage }}\ncharts={{ .CNIChartsPackage }}\n"
	activities := &Activities{cfg: config.Config{
		DgxcTenantUser:     "test-admin",
		DgxcTenantPassword: "test-password",
	}}

	tests := map[string]struct {
		cluster    db.Cluster
		wantImages string
		wantCharts string
	}{
		"Calico": {
			cluster:    db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
			wantImages: "nke-cni-calico-images-3.31.5",
			wantCharts: "nke-cni-calico-manifests-3.31.5",
		},
		"OVN-Kubernetes": {
			cluster:    db.Cluster{CniPlugin: db.CNIOvnKubernetes, CniVersion: "1.4.1"},
			wantImages: "nke-cni-ovn-images-1.4.1",
			wantCharts: "nke-cni-ovn-charts-1.4.1",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			userData, err := activities.renderUserData(
				context.Background(),
				userDataTemplate,
				db.Node{Name: "test-node"},
				"",
				"1.33.0",
				nil,
				params.cluster,
			)
			require.NoError(t, err)
			assert.Equal(t, "images="+params.wantImages+"\ncharts="+params.wantCharts+"\n", userData)
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
		)

		assert.NotContains(t, got, "node-pool:")
		assert.NotContains(t, got, uuid.Nil.String())
	})
}
