// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestClient_renderInfraMachine(t *testing.T) {
	profilesDir := t.TempDir()
	osProfileDir := filepath.Join(profilesDir, "osProfiles", "test-os")
	require.NoError(t, os.MkdirAll(osProfileDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(osProfileDir, "ipxeScript"), []byte("#!ipxe\n{{ .DiskSelectionStrategy }}"), 0o644))

	siteID := uuid.New()
	vpcID := uuid.New()
	instanceTypeID := uuid.New()
	subnetID := uuid.New()

	cluster := db.Cluster{
		ID:           uuid.New(),
		Name:         "test-cluster",
		ProviderType: db.ProviderTypeNico,
		Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID},
	}

	nodePool := db.NodePool{ID: uuid.New()}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: instanceTypeID,
		SubnetIDs:      []uuid.UUID{subnetID},
	})

	nodeSet := db.NodeSet{KubernetesVersion: "1.33.0"}
	nodeSet.SetNico(db.NicoNodePoolProvider{
		OSProfile:             "test-os",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
	})

	node := db.Node{ID: uuid.New(), Name: "worker-0"}

	t.Run("builds a NicoMachine for the node", func(t *testing.T) {
		obj, err := New().renderInfraMachine(profilesDir, "nico-test-cluster", node, nodeSet, nodePool, cluster)
		require.NoError(t, err)

		nicoMachine, ok := obj.(*nicov1.NicoMachine)
		require.True(t, ok, "expected *nicov1.NicoMachine, got %T", obj)
		assert.Equal(t, "worker-0", nicoMachine.Name)
		assert.Equal(t, "nico-test-cluster", nicoMachine.Namespace)
		assert.Equal(t, vpcID.String(), nicoMachine.Spec.VPCID)
		assert.Equal(t, instanceTypeID.String(), nicoMachine.Spec.InstanceTypeID)
		assert.Equal(t, "#!ipxe\nlargest", nicoMachine.Spec.IpxeScript)
		assert.Equal(t, node.ID.String(), nicoMachine.Spec.Labels[constants.LabelKeyNodeID])
	})

	t.Run("missing provider payload errors", func(t *testing.T) {
		_, err := New().renderInfraMachine(profilesDir, "ns", db.Node{}, db.NodeSet{}, db.NodePool{}, db.Cluster{})
		require.Error(t, err)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})
}

func TestResolveNodeConfig(t *testing.T) {
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

	controlPlaneNodeSet := db.NodeSet{
		KubernetesVersion: "1.32.1",
		TeleportEnabled:   true,
	}
	controlPlaneNodeSet.SetNico(db.NicoNodePoolProvider{
		OSProfile:             "cp-os",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})

	workerNodeSet := db.NodeSet{
		KubernetesVersion: "1.33.0",
		TeleportEnabled:   true,
	}
	workerNodeSet.SetNico(db.NicoNodePoolProvider{
		OSProfile:             "worker-os",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
	})

	type parameters struct {
		nodeSet  db.NodeSet
		nodePool db.NodePool
		want     NodeConfig
	}

	tests := map[string]parameters{
		"control plane uses resolved node set and node pool config": {
			nodeSet:  controlPlaneNodeSet,
			nodePool: controlPlaneNodePool,
			want: NodeConfig{
				OSProfile:             "cp-os",
				DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
				KubernetesVersion:     "1.32.1",
				TeleportEnabled:       true,
				InstanceTypeID:        controlPlaneInstanceTypeID,
				SSHKeyGroupIDs:        []uuid.UUID{uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
				SubnetIDs:             []uuid.UUID{controlPlaneSubnetID},
				NodePoolID:            uuid.Nil,
				IsControlPlaneRole:    true,
			},
		},
		"worker uses node set and node pool config": {
			nodeSet:  workerNodeSet,
			nodePool: workerNodePool,
			want: NodeConfig{
				OSProfile:                "worker-os",
				DiskSelectionStrategy:    db.DiskSelectionStrategyLargest,
				KubernetesVersion:        "1.33.0",
				TeleportEnabled:          true,
				InstanceTypeID:           workerInstanceTypeID,
				SSHKeyGroupIDs:           []uuid.UUID{uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
				VpcPrefixIDs:             []uuid.UUID{workerVpcPrefixID},
				NodePoolID:               workerNodePoolID,
				IsControlPlaneRole:       false,
				InfinibandPartitionID:    &infinibandPartitionID,
				NVLinkLogicalPartitionID: &nvLinkLogicalPartitionID,
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveNodeConfig(params.nodeSet, params.nodePool)
			require.NoError(t, err)
			assert.Equal(t, params.want, got)
		})
	}
}

func TestBuildMachineInterfaces(t *testing.T) {
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
			interfaces, err := buildMachineInterfaces(params.subnetIDs, params.vpcPrefixIDs)
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

func TestRenderIpxeScript(t *testing.T) {
	script, err := renderIpxeScript("set disk-strategy {{ .DiskSelectionStrategy }}\n", string(db.DiskSelectionStrategyLargest))
	require.NoError(t, err)
	assert.Equal(t, "set disk-strategy largest\n", script)
}

func TestBuildInstanceLabels(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	clusterID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	nodePoolID := uuid.MustParse("99999999-8888-7777-6666-555555555555")

	labels := BuildInstanceLabels(
		db.Node{ID: nodeID},
		db.Cluster{ID: clusterID},
		nodePoolID,
	)

	assert.Equal(t, map[string]string{
		constants.LabelKeyNodeID:     nodeID.String(),
		constants.LabelKeyClusterID:  clusterID.String(),
		constants.LabelKeyNodePoolID: nodePoolID.String(),
	}, labels)
}
