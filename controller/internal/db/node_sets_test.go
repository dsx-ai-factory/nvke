// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
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

func TestCreateNodeSet_ValidatesProviderPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.NodePool)(nil), (*db.NodeSet)(nil))
	dbImpl := db.NewDB(bunDB)

	cluster := newValidCluster(withName("nodeset-create-case"))
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	nodePool := db.NodePool{
		ID:                   uuid.New(),
		ClusterID:            cluster.ID,
		Name:                 "pool",
		NodeNamePrefix:       "w",
		SystemReservedMemory: "1Gi",
		KubernetesVersion:    "1.34.2",
		NodeLabels:           []string{},
		Nico:                 &db.NicoNodePoolProvider{},
	}
	_, err = bunDB.NewInsert().Model(&nodePool).Exec(ctx)
	require.NoError(t, err)

	t.Run("missing provider payload is rejected", func(t *testing.T) {
		_, err := dbImpl.CreateNodeSet(ctx, &db.NodeSet{ID: uuid.New(), NodePoolID: nodePool.ID})
		require.Error(t, err)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})

	t.Run("conflicting provider payload is rejected", func(t *testing.T) {
		nodeSet := &db.NodeSet{
			ID:         uuid.New(),
			NodePoolID: nodePool.ID,
			Nico:       &db.NicoNodePoolProvider{},
			Capi:       &db.CAPINodePoolProvider{},
		}
		_, err := dbImpl.CreateNodeSet(ctx, nodeSet)
		require.Error(t, err)
		require.Contains(t, err.Error(), "both nico and capi")
	})

	t.Run("valid payload is accepted", func(t *testing.T) {
		nodeSet := &db.NodeSet{ID: uuid.New(), NodePoolID: nodePool.ID}
		nodeSet.SetNico(db.NicoNodePoolProvider{})
		_, err := dbImpl.CreateNodeSet(ctx, nodeSet)
		require.NoError(t, err)
	})
}

func TestUpdateNodeSet_ValidatesProviderPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.NodePool)(nil), (*db.NodeSet)(nil))
	dbImpl := db.NewDB(bunDB)

	cluster := newValidCluster(withName("nodeset-update-case"))
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	nodePool := db.NodePool{
		ID:                   uuid.New(),
		ClusterID:            cluster.ID,
		Name:                 "pool",
		NodeNamePrefix:       "w",
		SystemReservedMemory: "1Gi",
		KubernetesVersion:    "1.34.2",
		NodeLabels:           []string{},
		Nico:                 &db.NicoNodePoolProvider{},
	}
	_, err = bunDB.NewInsert().Model(&nodePool).Exec(ctx)
	require.NoError(t, err)

	nodeSet := &db.NodeSet{ID: uuid.New(), NodePoolID: nodePool.ID}
	nodeSet.SetNico(db.NicoNodePoolProvider{})
	_, err = dbImpl.CreateNodeSet(ctx, nodeSet)
	require.NoError(t, err)

	t.Run("clearing the provider payload is rejected", func(t *testing.T) {
		stale := *nodeSet
		stale.Nico = nil
		_, err := dbImpl.UpdateNodeSet(ctx, &stale)
		require.Error(t, err)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})

	t.Run("setting both provider payloads is rejected", func(t *testing.T) {
		conflicted := *nodeSet
		conflicted.Capi = &db.CAPINodePoolProvider{}
		_, err := dbImpl.UpdateNodeSet(ctx, &conflicted)
		require.Error(t, err)
		require.Contains(t, err.Error(), "both nico and capi")
	})
}
