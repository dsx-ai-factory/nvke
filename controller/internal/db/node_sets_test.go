// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestNodeSetProviderSettersAndSync(t *testing.T) {
	subnetID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vpcPrefixID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	machineTemplate := json.RawMessage(`{"kind":"DockerMachineTemplate"}`)
	nodeSet := db.NodeSet{
		SubnetIDs:                  []uuid.UUID{subnetID},
		VpcPrefixIDs:               []uuid.UUID{vpcPrefixID},
		OSProfile:                  "nke-260318",
		ConfigProfile:              "gpu",
		SystemReservedMemory:       "32Gi",
		DiskSelectionStrategy:      db.DiskSelectionStrategyLargest,
		CapiInfraMachineTemplate:   machineTemplate,
		CapiBootstrapFiles:         []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}},
		CapiPreKubeadmCommands:     []string{"echo pre"},
		CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	}

	nodeSet.SyncProviderFields(db.ProviderTypeNico)

	require.Equal(t, []uuid.UUID{subnetID}, nodeSet.Nico.SubnetIDs)
	require.Nil(t, nodeSet.Capi)
	require.Equal(t, []uuid.UUID{vpcPrefixID}, nodeSet.Nico.VpcPrefixIDs)
	require.Equal(t, "nke-260318", nodeSet.Nico.OSProfile)
	require.Equal(t, "gpu", nodeSet.Nico.ConfigProfile)
	require.Equal(t, db.DiskSelectionStrategyLargest, nodeSet.Nico.DiskSelectionStrategy)

	nodeSet.SyncProviderFields(db.ProviderTypeCAPI)
	require.Nil(t, nodeSet.Nico)
	require.JSONEq(t, string(machineTemplate), string(nodeSet.Capi.InfraMachineTemplate))
	require.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, nodeSet.Capi.BootstrapFiles)
	require.Equal(t, []string{"echo pre"}, nodeSet.Capi.PreKubeadmCommands)
	require.Equal(t, db.SpireAttestationMethodJoinToken, nodeSet.Capi.SpireAttestationMethod)
}
