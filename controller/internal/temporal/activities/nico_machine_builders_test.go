// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	type parameters struct {
		createCtx *capiMachineCreateContext
		want      templateLessForgeCAPINodeConfig
	}

	tests := map[string]parameters{
		"control plane uses resolved node context for config": {
			createCtx: &capiMachineCreateContext{
				node: db.Node{InstanceTypeID: controlPlaneInstanceTypeID, ControlPlane: true},
				nodeSet: db.NodeSet{
					OSProfile:             "cp-os",
					DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
					KubernetesVersion:     "1.32.1",
					TeleportEnabled:       true,
				},
				nodePool: db.NodePool{
					InstanceTypeID: controlPlaneInstanceTypeID,
					SSHKeyGroupIDs: []uuid.UUID{uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
					SubnetIDs:      []uuid.UUID{controlPlaneSubnetID},
					ControlPlane:   true,
				},
				cluster: db.Cluster{
					ControlPlaneConfig: db.ControlPlaneConfig{
						ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
							// Deliberately different from the resolved node context to ensure no fallback drift.
							OSProfile:         "stale-cluster-control-plane-os",
							KubernetesVersion: "1.30.0",
							TeleportEnabled:   false,
							SSHKeyGroupIDs:    []uuid.UUID{uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")},
							SubnetIDs:         []uuid.UUID{uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")},
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
				nodeSet: db.NodeSet{
					OSProfile:             "worker-os",
					DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
					KubernetesVersion:     "1.33.0",
					TeleportEnabled:       true,
				},
				nodePool: db.NodePool{
					ID:                       workerNodePoolID,
					InstanceTypeID:           workerInstanceTypeID,
					SSHKeyGroupIDs:           []uuid.UUID{uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
					VpcPrefixIDs:             []uuid.UUID{workerVpcPrefixID},
					InfinibandPartitionID:    &infinibandPartitionID,
					NvLinkLogicalPartitionID: &nvLinkLogicalPartitionID,
				},
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

	createCtx := &capiMachineCreateContext{
		node: db.Node{
			InstanceTypeID: controlPlaneInstanceTypeID,
			ControlPlane:   true,
		},
		nodeSet: db.NodeSet{
			OSProfile:             "cp-os-a",
			DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
			KubernetesVersion:     "1.33.7",
			TeleportEnabled:       true,
		},
		nodePool: db.NodePool{
			// Synthetic no-nodepool control-plane context from nodes.GetNodeContext.
			ID:             uuid.Nil,
			InstanceTypeID: controlPlaneInstanceTypeID,
			SSHKeyGroupIDs: []uuid.UUID{pinnedSSHKeyGroupID},
			SubnetIDs:      []uuid.UUID{pinnedSubnetID},
			ControlPlane:   true,
		},
		cluster: db.Cluster{
			ControlPlaneConfig: db.ControlPlaneConfig{
				ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
					OSProfile:         "cp-os-b",
					KubernetesVersion: "1.40.1",
					TeleportEnabled:   false,
					SSHKeyGroupIDs:    []uuid.UUID{uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")},
					SubnetIDs:         []uuid.UUID{uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001")},
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

	tests := map[string]parameters{
		"fails fast when resolved context is missing": {
			createCtx: &capiMachineCreateContext{
				node: db.Node{InstanceTypeID: uuid.New()},
			},
			wantErrContain: "resolved node context is incomplete",
		},
		"requires os profile": {
			createCtx: &capiMachineCreateContext{
				nodeSet: db.NodeSet{
					KubernetesVersion: "1.33.0",
				},
				nodePool: db.NodePool{
					InstanceTypeID: uuid.New(),
					SubnetIDs:      []uuid.UUID{uuid.New()},
				},
			},
			wantErrContain: "nodeSet.osProfile",
		},
		"requires instance type": {
			createCtx: &capiMachineCreateContext{
				nodeSet: db.NodeSet{
					OSProfile:         "cp-os",
					KubernetesVersion: "1.33.0",
				},
				nodePool: db.NodePool{
					SubnetIDs: []uuid.UUID{uuid.New()},
				},
			},
			wantErrContain: "nodePool.instanceTypeID",
		},
		"requires network candidates": {
			createCtx: &capiMachineCreateContext{
				nodeSet: db.NodeSet{
					OSProfile:         "cp-os",
					KubernetesVersion: "1.33.0",
				},
				nodePool: db.NodePool{
					InstanceTypeID: uuid.New(),
				},
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

func TestNicoMachineBuilders_BuildNicoMachineInfinibandInterfaces(t *testing.T) {
	instanceTypeID := uuid.New()
	infinibandPartitionID := uuid.New()
	instanceType := GetInstanceTypeOutput{
		InfiniBandDeviceName:      "ib0",
		InfiniBandActiveDeviceIDs: []int32{1, 3},
	}

	infinibandInterfaces, err := buildNicoMachineInfinibandInterfaces(instanceTypeID, &infinibandPartitionID, instanceType)
	require.NoError(t, err)
	require.Len(t, infinibandInterfaces, 2)
	assert.Equal(t, infinibandPartitionID.String(), infinibandInterfaces[0].PartitionID)
	assert.Equal(t, "ib0", infinibandInterfaces[0].Device)
	assert.Equal(t, int32(1), *infinibandInterfaces[0].DeviceInstance)
	assert.True(t, *infinibandInterfaces[0].IsPhysical)
}

func TestNicoMachineBuilders_BuildNicoMachineNVLinkInterfaces(t *testing.T) {
	instanceTypeID := uuid.New()
	nvLinkLogicalPartitionID := uuid.New()
	instanceType := GetInstanceTypeOutput{
		NVLinkInterfaceCount:  2,
		NVLinkActiveDeviceIDs: []int32{0, 2},
	}

	nvLinkInterfaces, err := buildNicoMachineNVLinkInterfaces(instanceTypeID, &nvLinkLogicalPartitionID, instanceType)
	require.NoError(t, err)
	require.Len(t, nvLinkInterfaces, 2)
	assert.Equal(t, nvLinkLogicalPartitionID.String(), nvLinkInterfaces[0].NVLinkLogicalPartitionID)
	assert.Equal(t, int32(0), *nvLinkInterfaces[0].DeviceInstance)
	assert.Equal(t, int32(2), *nvLinkInterfaces[1].DeviceInstance)
}

func TestNicoMachineBuilders_ValidateNicoMachineAcceleratorInterfaces(t *testing.T) {
	instanceTypeID := uuid.New()
	partitionID := uuid.New()

	type parameters struct {
		build          func() error
		wantErrContain string
	}

	tests := map[string]parameters{
		"requires infiniband support": {
			build: func() error {
				_, err := buildNicoMachineInfinibandInterfaces(instanceTypeID, &partitionID, GetInstanceTypeOutput{})
				return err
			},
			wantErrContain: "does not support InfiniBand",
		},
		"requires nvlink support": {
			build: func() error {
				_, err := buildNicoMachineNVLinkInterfaces(instanceTypeID, &partitionID, GetInstanceTypeOutput{})
				return err
			},
			wantErrContain: "does not support NVLink Interface",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, params.build(), params.wantErrContain)
		})
	}
}
