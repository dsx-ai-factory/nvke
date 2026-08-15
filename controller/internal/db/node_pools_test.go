// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestNodePoolProviderGettersSettersAndSync(t *testing.T) {
	subnetID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vpcPrefixID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	instanceTypeID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	ibPartitionID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	nvlinkPartitionID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	sshKeyGroupID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	machineTemplate := json.RawMessage(`{"kind":"ForgeMachineTemplate"}`)
	nodePool := db.NodePool{
		SubnetIDs:                  []uuid.UUID{subnetID},
		VpcPrefixIDs:               []uuid.UUID{vpcPrefixID},
		InstanceTypeID:             instanceTypeID,
		InfinibandPartitionID:      &ibPartitionID,
		NvLinkLogicalPartitionID:   &nvlinkPartitionID,
		SSHKeyGroupIDs:             []uuid.UUID{sshKeyGroupID},
		OSProfile:                  "nke-260318",
		ConfigProfile:              "gpu",
		SystemReservedMemory:       "32Gi",
		DiskSelectionStrategy:      db.DiskSelectionStrategyLargest,
		CapiInfraMachineTemplate:   machineTemplate,
		CapiBootstrapFiles:         []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}},
		CapiPreKubeadmCommands:     []string{"echo pre"},
		CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	}

	nodePool.SyncProviderFields(db.ProviderTypeNico)

	require.Equal(t, []uuid.UUID{subnetID}, nodePool.Nico.SubnetIDs)
	require.Nil(t, nodePool.Capi)
	require.Equal(t, []uuid.UUID{vpcPrefixID}, nodePool.Nico.VpcPrefixIDs)
	require.Equal(t, instanceTypeID, nodePool.Nico.InstanceTypeID)
	require.Equal(t, ibPartitionID, *nodePool.Nico.InfinibandPartitionID)
	require.Equal(t, nvlinkPartitionID, *nodePool.Nico.NvLinkLogicalPartitionID)
	require.Equal(t, []uuid.UUID{sshKeyGroupID}, nodePool.Nico.SSHKeyGroupIDs)
	require.Equal(t, "nke-260318", nodePool.Nico.OSProfile)
	require.Equal(t, "gpu", nodePool.Nico.ConfigProfile)
	require.Equal(t, db.DiskSelectionStrategyLargest, nodePool.Nico.DiskSelectionStrategy)

	nicoNodePoolProvider, err := nodePool.NicoProvider()
	require.NoError(t, err)
	require.Equal(t, instanceTypeID, nicoNodePoolProvider.InstanceTypeID)
	_, err = nodePool.CAPIProvider()
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrMissingProviderPayload)

	nodePool.SyncProviderFields(db.ProviderTypeCAPI)
	require.Nil(t, nodePool.Nico)
	require.JSONEq(t, string(machineTemplate), string(nodePool.Capi.InfraMachineTemplate))
	require.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, nodePool.Capi.BootstrapFiles)
	require.Equal(t, []string{"echo pre"}, nodePool.Capi.PreKubeadmCommands)
	require.Equal(t, db.SpireAttestationMethodJoinToken, nodePool.Capi.SpireAttestationMethod)

	capiNodePoolProvider, err := nodePool.CAPIProvider()
	require.NoError(t, err)
	require.JSONEq(t, string(machineTemplate), string(capiNodePoolProvider.InfraMachineTemplate))
	_, err = nodePool.NicoProvider()
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrMissingProviderPayload)

	nextSubnetID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	nextInstanceTypeID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	nodePool.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:             []uuid.UUID{nextSubnetID},
		InstanceTypeID:        nextInstanceTypeID,
		OSProfile:             "next-os",
		ConfigProfile:         "next-config",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	require.Equal(t, nextInstanceTypeID, nodePool.InstanceTypeID)
	require.Equal(t, "next-os", nodePool.OSProfile)
	require.Equal(t, "next-config", nodePool.ConfigProfile)
	require.Equal(t, "32Gi", nodePool.SystemReservedMemory)
}
