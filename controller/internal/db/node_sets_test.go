// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
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

func TestNodeSetResolveProviderType(t *testing.T) {
	nico := db.NodeSet{ID: uuid.New()}
	nico.SetNico(db.NicoNodePoolProvider{})
	providerType, err := nico.ResolveProviderType()
	require.NoError(t, err)
	require.Equal(t, db.ProviderTypeNico, providerType)

	capi := db.NodeSet{ID: uuid.New()}
	capi.SetCapi(db.CAPINodePoolProvider{})
	providerType, err = capi.ResolveProviderType()
	require.NoError(t, err)
	require.Equal(t, db.ProviderTypeCAPI, providerType)

	empty := db.NodeSet{ID: uuid.New()}
	_, err = empty.ResolveProviderType()
	require.ErrorIs(t, err, db.ErrMissingProviderPayload)
}

func TestNodePoolNeedsNewNodeSet(t *testing.T) {
	currentID := uuid.New()
	basePool := db.NodePool{
		CurrentNodeSetID:     &currentID,
		KubernetesVersion:    "1.34.2",
		SystemReservedMemory: "1Gi",
		KubeReservedMemory:   "512Mi",
		TeleportEnabled:      true,
		Nico: &db.NicoNodePoolProvider{
			OSProfile:     "os-v1",
			ConfigProfile: "cfg-v1",
		},
	}
	current := db.NodeSet{
		ID:                   currentID,
		KubernetesVersion:    "1.34.2",
		SystemReservedMemory: "1Gi",
		KubeReservedMemory:   "512Mi",
		TeleportEnabled:      true,
		Nico: &db.NicoNodePoolProvider{
			OSProfile:     "os-v1",
			ConfigProfile: "cfg-v1",
		},
	}

	tests := []struct {
		name string
		pool db.NodePool
		want bool
	}{
		{name: "matching configuration", pool: basePool},
		{name: "max surge alone", pool: func() db.NodePool {
			pool := basePool
			pool.MaxSurge = 1
			return pool
		}()},
		{name: "config profile changed", pool: func() db.NodePool {
			pool := basePool
			provider := *pool.Nico
			provider.ConfigProfile = "cfg-v2"
			pool.Nico = &provider
			return pool
		}(), want: true},
		{name: "reserved memory changed", pool: func() db.NodePool {
			pool := basePool
			pool.SystemReservedMemory = "2Gi"
			return pool
		}(), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, db.NodePoolNeedsNewNodeSet(tc.pool, current))
		})
	}
}
