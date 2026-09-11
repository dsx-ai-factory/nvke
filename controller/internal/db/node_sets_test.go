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

func TestNodeSetProviderSetters(t *testing.T) {
	subnetID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vpcPrefixID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	machineTemplate := json.RawMessage(`{"kind":"DockerMachineTemplate"}`)
	nodeSet := db.NodeSet{SystemReservedMemory: "32Gi"}

	nodeSet.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:             []uuid.UUID{subnetID},
		VpcPrefixIDs:          []uuid.UUID{vpcPrefixID},
		OSProfile:             "nke-260318",
		ConfigProfile:         "gpu",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
	})

	require.Equal(t, []uuid.UUID{subnetID}, nodeSet.Nico.SubnetIDs)
	require.Nil(t, nodeSet.Capi)
	require.Equal(t, []uuid.UUID{vpcPrefixID}, nodeSet.Nico.VpcPrefixIDs)
	require.Equal(t, "nke-260318", nodeSet.Nico.OSProfile)
	require.Equal(t, "gpu", nodeSet.Nico.ConfigProfile)
	require.Equal(t, db.DiskSelectionStrategyLargest, nodeSet.Nico.DiskSelectionStrategy)

	// Setting one provider clears the other, so a node set never carries both.
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   machineTemplate,
		BootstrapFiles:         []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}},
		PreKubeadmCommands:     []string{"echo pre"},
		SpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	})
	require.Nil(t, nodeSet.Nico)
	require.JSONEq(t, string(machineTemplate), string(nodeSet.Capi.InfraMachineTemplate))
	require.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, nodeSet.Capi.BootstrapFiles)
	require.Equal(t, []string{"echo pre"}, nodeSet.Capi.PreKubeadmCommands)
	require.Equal(t, db.SpireAttestationMethodJoinToken, nodeSet.Capi.SpireAttestationMethod)
}
