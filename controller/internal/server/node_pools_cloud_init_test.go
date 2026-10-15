// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestValidateCreateNodePoolRequest_CAPICloudInitInputs(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	req := &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			BootstrapFiles: []api.CloudInitFile{
				{Path: "/etc/custom.conf", Content: "hello"},
			},
			PreKubeadmCommands: []string{"echo before kubeadm"},
		},
	}
	err := service.validateCreateNodePoolRequest(capiCluster(), req)
	require.NoError(t, err)
}

func TestValidateCreateNodePoolRequest_CAPIAllowsMissingConfigProfile(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	req := &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		},
	}
	err := service.validateCreateNodePoolRequest(capiCluster(), req)
	require.NoError(t, err)
}

func TestValidateCreateNodePoolRequest_CAPIRejectsNicoPayload(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	err := service.validateCreateNodePoolRequest(capiCluster(), &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Nico:             &api.NicoNodePoolProvider{OSProfile: "ubuntu"},
	})
	require.EqualError(t, err, "nico is not supported, please provide infra")
}

func TestValidateCreateNodePoolRequest_ForgeRejectsCAPICloudInitInputs(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Nico: &api.NicoNodePoolProvider{
			InstanceTypeID: uuid.New(),
			OSProfile:      "ubuntu",
			ConfigProfile:  "default",
		},
		Infra: &api.InfraNodePoolProvider{
			BootstrapFiles:     []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
			PreKubeadmCommands: []string{"echo before kubeadm"},
		},
	})
	require.EqualError(t, err, "infra is not supported, please provide nico")
}

func TestValidateCreateNodePoolRequest_ForgeConfigProfileIsOptional(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	req := &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Nico: &api.NicoNodePoolProvider{
			InstanceTypeID: uuid.New(),
			OSProfile:      "ubuntu",
			SubnetIDs:      []uuid.UUID{uuid.New()},
		},
	}
	err := service.validateCreateNodePoolRequest(forgeCluster(), req)
	require.NoError(t, err)
}

func TestValidateUpdatedNodePool_RejectsManagedSpirePathsWhenSpireEnabled(t *testing.T) {
	req := &api.CreateNodePoolRequest{
		ClusterID:        uuid.New(),
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			BootstrapFiles: []api.CloudInitFile{
				{Path: "/etc/spire-agent/agent.conf", Content: "override"},
			},
		},
	}
	dbNodePool, err := dbNodePoolFromAPI(req, "1.32.0")
	require.NoError(t, err)

	err = validateUpdatedNodePool(capiCluster(), &dbNodePool)
	require.EqualError(t, err, `file path "/etc/spire-agent/agent.conf" is managed by the controller and must not be provided`)
}

func TestDBNodePoolFromAPI_CopiesCAPICloudInitInputs(t *testing.T) {
	req := &api.CreateNodePoolRequest{
		ClusterID:        uuid.New(),
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate:    map[string]interface{}{"apiVersion": "infra.example.com/v1", "kind": "ExampleMachineTemplate"},
			BootstrapFiles:     []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
			PreKubeadmCommands: []string{"echo before kubeadm"},
		},
	}
	dbNodePool, err := dbNodePoolFromAPI(req, "1.32.0")
	require.NoError(t, err)
	capiProvider, err := dbNodePool.CAPIProvider()
	require.NoError(t, err)
	require.Equal(t, []db.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}}, capiProvider.BootstrapFiles)
	require.Equal(t, []string{"echo before kubeadm"}, capiProvider.PreKubeadmCommands)
}

// capiCluster and forgeCluster build the minimal stored cluster shape the node
// pool validators dispatch on: the provider discriminator, plus the Forge
// identity fields a nico cluster always carries.
func capiCluster() db.Cluster {
	return db.Cluster{ProviderType: db.ProviderTypeCAPI}
}

func forgeCluster() db.Cluster {
	return db.Cluster{
		ProviderType: db.ProviderTypeNico,
		Nico: &db.NicoClusterProvider{
			OrgID:  "test-org",
			SiteID: uuid.New(),
		},
	}
}

func TestDbNodePoolFromAPI_EveryNestedNicoFieldReachesTheStoredPool(t *testing.T) {
	subnet := uuid.New()
	vpcPrefix := uuid.New()
	instanceType := uuid.New()
	ib := uuid.New()
	nv := uuid.New()
	ssh := uuid.New()

	req := &api.CreateNodePoolRequest{
		ClusterID:      uuid.New(),
		Name:           "worker",
		NodeNamePrefix: "w",
		Nico: &api.NicoNodePoolProvider{
			SubnetIDs:                []uuid.UUID{subnet},
			VpcPrefixIDs:             []uuid.UUID{vpcPrefix},
			InstanceTypeID:           instanceType,
			InfinibandPartitionID:    &ib,
			NvLinkLogicalPartitionID: &nv,
			SSHKeyGroupIDs:           &[]uuid.UUID{ssh},
			OSProfile:                "os",
			ConfigProfile:            "cfg",
			DiskSelectionStrategy:    api.DiskSelectionStrategyCreateLargest,
		},
	}

	pool, err := dbNodePoolFromAPI(req, "1.31.0")
	require.NoError(t, err)
	require.NotNil(t, pool.Nico)
	assert.Nil(t, pool.Capi)

	nico := pool.Nico
	assert.Equal(t, []uuid.UUID{subnet}, nico.SubnetIDs)
	assert.Equal(t, []uuid.UUID{vpcPrefix}, nico.VpcPrefixIDs)
	assert.Equal(t, instanceType, nico.InstanceTypeID)
	require.NotNil(t, nico.InfinibandPartitionID)
	assert.Equal(t, ib, *nico.InfinibandPartitionID)
	require.NotNil(t, nico.NvLinkLogicalPartitionID)
	assert.Equal(t, nv, *nico.NvLinkLogicalPartitionID)
	assert.Equal(t, []uuid.UUID{ssh}, nico.SSHKeyGroupIDs)
	assert.Equal(t, "os", nico.OSProfile)
	assert.Equal(t, "cfg", nico.ConfigProfile)
	assert.Equal(t, db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateLargest), nico.DiskSelectionStrategy)

	got := apiNodePoolFromDB(&pool)
	require.NotNil(t, got.Nico)
	assert.Equal(t, "os", got.Nico.OSProfile)
	assert.Nil(t, got.Infra)
}

func TestDbNodePoolFromAPI_EveryNestedCapiFieldReachesTheStoredPool(t *testing.T) {
	tmpl := map[string]interface{}{"kind": "DockerMachineTemplate"}
	files := []api.CloudInitFile{{Path: "/tmp/a", Content: "x"}}

	req := &api.CreateNodePoolRequest{
		ClusterID:      uuid.New(),
		Name:           "worker",
		NodeNamePrefix: "w",
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate:    tmpl,
			BootstrapFiles:     files,
			PreKubeadmCommands: []string{"echo hi"},
		},
	}

	pool, err := dbNodePoolFromAPI(req, "1.31.0")
	require.NoError(t, err)
	require.NotNil(t, pool.Capi)
	assert.Nil(t, pool.Nico)
	assert.Empty(t, pool.Capi.SpireAttestationMethod)
	assert.Equal(t, []string{"echo hi"}, pool.Capi.PreKubeadmCommands)
	assert.Equal(t, []db.CloudInitFile{{Path: "/tmp/a", Content: "x"}}, pool.Capi.BootstrapFiles)

	got := apiNodePoolFromDB(&pool)
	require.NotNil(t, got.Infra)
	assert.Equal(t, "DockerMachineTemplate", got.Infra.MachineTemplate["kind"])
	assert.Equal(t, files, got.Infra.BootstrapFiles)
	assert.Nil(t, got.Nico)
}

func TestApplyUpdateNodePoolRequest_NestedNicoPatch(t *testing.T) {
	subnetA := uuid.New()
	subnetB := uuid.New()
	newForgePool := func() *db.NodePool {
		pool := &db.NodePool{
			ID:                uuid.New(),
			ClusterID:         uuid.New(),
			Name:              "worker",
			NodeNamePrefix:    "w",
			KubernetesVersion: "1.31.0",
			NodeLabels:        map[string]string{},
			Conditions:        db.NodePoolConditions{},
		}
		pool.SetNico(db.NicoNodePoolProvider{
			SubnetIDs:             []uuid.UUID{subnetA},
			VpcPrefixIDs:          []uuid.UUID{},
			InstanceTypeID:        uuid.New(),
			SSHKeyGroupIDs:        []uuid.UUID{},
			OSProfile:             "os",
			ConfigProfile:         "cfg",
			DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
		})
		return pool
	}

	t.Run("a value replaces the stored one", func(t *testing.T) {
		pool := newForgePool()
		req := &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{
				SubnetIDs: []uuid.UUID{subnetB},
				OSProfile: ptrString("os-v2"),
			},
		}
		require.NoError(t, applyUpdateNodePoolRequest(forgeCluster(), pool, req))

		require.NotNil(t, pool.Nico)
		assert.Equal(t, []uuid.UUID{subnetB}, pool.Nico.SubnetIDs)
		assert.Equal(t, "os-v2", pool.Nico.OSProfile)

		got := apiNodePoolFromDB(pool)
		require.NotNil(t, got.Nico)
		assert.Equal(t, []uuid.UUID{subnetB}, got.Nico.SubnetIDs)
		assert.Equal(t, "os-v2", got.Nico.OSProfile)
	})

	t.Run("an empty list clears the stored one", func(t *testing.T) {
		pool := newForgePool()
		req := &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []uuid.UUID{}},
		}
		require.NoError(t, applyUpdateNodePoolRequest(forgeCluster(), pool, req))

		require.NotNil(t, pool.Nico)
		assert.Empty(t, pool.Nico.SubnetIDs)
	})

	t.Run("an omitted field leaves the stored one alone", func(t *testing.T) {
		pool := newForgePool()
		req := &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{ConfigProfile: ptrString("cfg-v2")},
		}
		require.NoError(t, applyUpdateNodePoolRequest(forgeCluster(), pool, req))

		require.NotNil(t, pool.Nico)
		assert.Equal(t, "cfg-v2", pool.Nico.ConfigProfile)
		assert.Equal(t, "os", pool.Nico.OSProfile)
		assert.Equal(t, []uuid.UUID{subnetA}, pool.Nico.SubnetIDs)
	})
}

func TestApiNodePoolFromDB_EmitsNestedWhenPayloadPresent(t *testing.T) {
	pool := &db.NodePool{
		ID:                uuid.New(),
		ClusterID:         uuid.New(),
		Name:              "worker",
		NodeNamePrefix:    "w",
		KubernetesVersion: "1.31.0",
		NodeLabels:        map[string]string{},
		Conditions:        db.NodePoolConditions{},
		Nico: &db.NicoNodePoolProvider{
			InstanceTypeID:        uuid.New(),
			SSHKeyGroupIDs:        []uuid.UUID{},
			OSProfile:             "os",
			ConfigProfile:         "cfg",
			SubnetIDs:             []uuid.UUID{uuid.New()},
			VpcPrefixIDs:          []uuid.UUID{},
			DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
		},
	}

	got := apiNodePoolFromDB(pool)
	require.NotNil(t, got.Nico)
	assert.Equal(t, "os", got.Nico.OSProfile)
	assert.Nil(t, got.Infra)
}

func TestNodePoolGetThenPatchNestedMutableOnly(t *testing.T) {
	// A GET echoes the whole nico object, including the immutable fields; a PATCH
	// takes only the mutable subset. Pin that a stripped-down patch round-trips.
	// The rejection of an unstripped echo is covered by
	// TestOpenAPIValidator_UpdateNodePool, since the update schema is what rejects it.
	pool := &db.NodePool{
		ID:                uuid.New(),
		ClusterID:         uuid.New(),
		Name:              "worker",
		NodeNamePrefix:    "w",
		KubernetesVersion: "1.31.0",
		NodeLabels:        map[string]string{},
		Conditions:        db.NodePoolConditions{},
	}
	pool.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:             []uuid.UUID{uuid.New()},
		VpcPrefixIDs:          []uuid.UUID{},
		InstanceTypeID:        uuid.New(),
		SSHKeyGroupIDs:        []uuid.UUID{uuid.New()},
		OSProfile:             "os",
		ConfigProfile:         "cfg",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	got := apiNodePoolFromDB(pool)
	require.NotNil(t, got.Nico)

	assert.NotEqual(t, uuid.Nil, got.Nico.InstanceTypeID)

	t.Run("mutable-only nested patch is accepted", func(t *testing.T) {
		newSubnet := uuid.New()
		req := &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{
				SubnetIDs: []uuid.UUID{newSubnet},
				OSProfile: ptrString("os-v2"),
			},
		}
		require.NoError(t, applyUpdateNodePoolRequest(forgeCluster(), pool, req))
		assert.Equal(t, []uuid.UUID{newSubnet}, pool.Nico.SubnetIDs)
		assert.Equal(t, "os-v2", pool.Nico.OSProfile)
	})
}
