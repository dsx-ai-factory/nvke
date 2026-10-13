// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/naming"
)

// A CAPI cluster rejects a nico payload whatever it carries: the provider shape
// is what is unsupported, so every field spelling reaches the same rejection.
func TestValidateCreateNodePoolRequest_CAPIRejectsNicoPayloadWhateverItCarries(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	cases := []struct {
		name string
		nico *api.NicoNodePoolProvider
	}{
		{name: "instanceTypeID", nico: &api.NicoNodePoolProvider{InstanceTypeID: uuid.New()}},
		{name: "explicit empty subnetIDs", nico: &api.NicoNodePoolProvider{SubnetIDs: []uuid.UUID{}}},
		{
			name: "diskSelectionStrategy",
			nico: &api.NicoNodePoolProvider{DiskSelectionStrategy: api.DiskSelectionStrategyCreateLargest},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := service.validateCreateNodePoolRequest(capiCluster(), &api.CreateNodePoolRequest{
				Name:             "worker",
				NodeNamePrefix:   "worker",
				DesiredNodeCount: 1,
				Nico:             tc.nico,
			})

			require.EqualError(t, err, "nico is not supported, please provide infra")
		})
	}
}

// The mirror of the above: a nico cluster rejects an infra payload whatever it
// carries, even alongside an otherwise valid nico payload.
func TestValidateCreateNodePoolRequest_NicoRejectsInfraPayloadWhateverItCarries(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	cases := []struct {
		name  string
		infra *api.InfraNodePoolProvider
	}{
		{
			name:  "empty machineTemplate",
			infra: &api.InfraNodePoolProvider{MachineTemplate: map[string]interface{}{}},
		},
		{name: "empty infra payload", infra: &api.InfraNodePoolProvider{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
				Name:             "worker",
				NodeNamePrefix:   "worker",
				DesiredNodeCount: 1,
				Nico: &api.NicoNodePoolProvider{
					InstanceTypeID: uuid.New(),
					OSProfile:      "ubuntu",
					ConfigProfile:  "default",
				},
				Infra: tc.infra,
			})

			require.EqualError(t, err, "infra is not supported, please provide nico")
		})
	}
}

func TestValidateCreateNodePoolRequest_RejectsUnsupportedParentProvider(t *testing.T) {
	t.Run("capi cluster requires infra support", func(t *testing.T) {
		service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderNico}, false)

		err := service.validateCreateNodePoolRequest(capiCluster(), &api.CreateNodePoolRequest{
			Name:             "worker",
			NodeNamePrefix:   "worker",
			DesiredNodeCount: 1,
			Infra: &api.InfraNodePoolProvider{
				MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			},
		})

		require.EqualError(t, err, "infra provider is not supported by this deployment")
	})

	t.Run("nico cluster requires nico support", func(t *testing.T) {
		service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderInfra}, false)

		err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
			Name:             "worker",
			NodeNamePrefix:   "worker",
			DesiredNodeCount: 1,
			Nico: &api.NicoNodePoolProvider{
				SubnetIDs:      []uuid.UUID{uuid.New()},
				InstanceTypeID: uuid.New(),
				OSProfile:      "ubuntu",
				ConfigProfile:  "default",
			},
		})

		require.EqualError(t, err, "nico provider is not supported by this deployment")
	})
}

func TestValidateCreateNodePoolRequest_DisabledProviderPrecedesProviderShape(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderNico}, false)

	err := service.validateCreateNodePoolRequest(capiCluster(), &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Nico: &api.NicoNodePoolProvider{
			InstanceTypeID: uuid.New(),
		},
	})

	require.EqualError(t, err, "infra provider is not supported by this deployment")
}

func TestValidateUpdateNodePoolRequest_DisabledProviderOnlyBlocksProviderPayloads(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderNico}, false)
	cluster := db.Cluster{ProviderType: db.ProviderTypeCAPI}

	require.NoError(t, service.validateUpdateNodePoolRequest(cluster, &api.UpdateNodePoolRequest{}))

	err := service.validateUpdateNodePoolRequest(cluster, &api.UpdateNodePoolRequest{
		Infra: &api.InfraNodePoolProviderUpdate{},
	})
	require.EqualError(t, err, "infra provider is not supported by this deployment")

	err = service.validateUpdateNodePoolRequest(cluster, &api.UpdateNodePoolRequest{
		Nico: &api.NicoNodePoolProviderUpdate{},
	})
	require.EqualError(t, err, "infra provider is not supported by this deployment")
}

func TestValidateUpdateNodePoolRequest_CAPIRejectsForgeSpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	t.Run("nico provider shape", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeCAPI, Nico: &db.NicoClusterProvider{SiteID: uuid.Nil}}, &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []uuid.UUID{}},
		})

		require.EqualError(t, err, "nico is not supported, please use infra")
	})

	t.Run("empty infra.machineTemplate", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeCAPI, Nico: &db.NicoClusterProvider{SiteID: uuid.Nil}}, &api.UpdateNodePoolRequest{
			Infra: &api.InfraNodePoolProviderUpdate{MachineTemplate: map[string]interface{}{}},
		})

		require.EqualError(t, err, "infra.machineTemplate cannot be empty")
	})

	t.Run("nico diskSelectionStrategy", func(t *testing.T) {
		largest := api.DiskSelectionStrategyLargest
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeCAPI, Nico: &db.NicoClusterProvider{SiteID: uuid.Nil}}, &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{DiskSelectionStrategy: &largest},
		})

		require.EqualError(t, err, "nico is not supported, please use infra")
	})
}

func TestValidateUpdateNodePoolRequest_ForgeRejectsCAPISpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	t.Run("infra.bootstrapFiles", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeNico, Nico: &db.NicoClusterProvider{SiteID: uuid.New()}}, &api.UpdateNodePoolRequest{
			Infra: &api.InfraNodePoolProviderUpdate{
				BootstrapFiles: []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
			},
		})

		require.EqualError(t, err, "infra is not supported, please use nico")
	})
}

func TestValidateUpdatedNodePool_CAPIRequiresEmptyDiskSelectionStrategy(t *testing.T) {
	pool := &db.NodePool{}
	pool.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate: []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ExampleMachineTemplate"}`),
	})
	// A pool carrying both payloads is unreachable through the API — the update
	// request for a CAPI pool rejects a nico block outright — so the mixed state
	// this check guards against has to be assembled by hand.
	pool.Nico = &db.NicoNodePoolProvider{
		DiskSelectionStrategy: db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateLargest),
	}

	err := validateUpdatedNodePool(capiCluster(), pool)

	require.EqualError(t, err, "nico.diskSelectionStrategy is not supported")
}

func TestValidateCreateNodePoolRequest_RejectsTaintsOnControlPlanePool(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
		Name:           "cp",
		NodeNamePrefix: "cp",
		ControlPlane:   true,
		NodeTaints: []api.NodeTaint{{
			Key:    "dedicated",
			Effect: api.NodeTaintEffectNoSchedule,
		}},
	})

	require.EqualError(t, err, "nodeTaints are not allowed on a control-plane pool")
}

func TestValidateCreateNodePoolRequest_NodeNamePrefixLength(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)
	clusterName := "runtime-dgxc-k8s-forge-ytl-dev1"

	capiNodePoolRequest := func(nodeNamePrefix string) *api.CreateNodePoolRequest {
		return &api.CreateNodePoolRequest{
			Name:             "worker",
			NodeNamePrefix:   nodeNamePrefix,
			DesiredNodeCount: 1,
			Infra: &api.InfraNodePoolProvider{
				MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			},
		}
	}
	cluster := capiCluster()
	cluster.Name = clusterName

	t.Run("accepts short prefix", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(cluster, capiNodePoolRequest("cp"))

		require.NoError(t, err)
	})

	t.Run("rejects prefix that exceeds generated node name limit", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(
			cluster,
			capiNodePoolRequest("runtime-dgxc-k8s-forge-ytl-dev1-cp"),
		)

		require.Error(t, err)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestValidateCreateClusterRequest_NodeNamePrefixLength(t *testing.T) {
	clusterName := "runtime-dgxc-k8s-forge-ytl-dev1"
	svc := clusterService{}

	forgeClusterRequest := func(name, nodeNamePrefix string) *api.CreateClusterRequest {
		nodePool := validForgeNodePoolCreateConfig("cp")
		nodePool.NodeNamePrefix = nodeNamePrefix
		return &api.CreateClusterRequest{
			Name: name,
			Nico: &api.NicoClusterProvider{
				OrgID:  "test-org",
				SiteID: uuid.New(),
				VpcID:  uuid.New(),
			},
			CniPlugin:                     "ovn-kubernetes",
			ControlPlaneKubernetesVersion: "1.29.0",
			NodePools:                     &[]api.NodePoolCreateConfig{nodePool},
		}
	}

	t.Run("rejects cluster name too long for generated node names", func(t *testing.T) {
		err := svc.ValidateCreateClusterRequestFields(
			forgeClusterRequest(strings.Repeat("a", naming.MaxGeneratedNodeNameLength), "cp"),
		)

		require.Error(t, err)
		require.Contains(t, err.Error(), "cluster name")
		require.Contains(t, err.Error(), "too long")
		require.Contains(t, err.Error(), naming.GeneratedNodeNameFormat)
		require.NotContains(t, err.Error(), `invalid node pool`)
	})

	t.Run("rejects embedded node pool with prefix too long for cluster name", func(t *testing.T) {
		err := svc.ValidateCreateClusterRequestFields(
			forgeClusterRequest(clusterName, "runtime-dgxc-k8s-forge-ytl-dev1-cp"),
		)

		require.Error(t, err)
		require.Contains(t, err.Error(), `invalid node pool "cp"`)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestValidateUpdateNodePoolRequest_WrongProvider(t *testing.T) {
	svc := &nodePoolService{}
	req := &api.UpdateNodePoolRequest{
		Nico: &api.NicoNodePoolProviderUpdate{OSProfile: ptrString("os")},
	}
	err := svc.validateUpdateNodePoolRequest(capiCluster(), req)
	require.EqualError(t, err, "nico is not supported, please use infra")
}

// Clearing the OS profile is expressible in the request, and rejected: a Forge
// node pool always has one. The config profile is optional and may be cleared.
func TestValidateUpdateForgeNodePoolRequest_RejectsClearedOSProfile(t *testing.T) {
	svc := &nodePoolService{}
	cases := []struct {
		name string
		req  *api.UpdateNodePoolRequest
		want string
	}{
		{
			name: "osProfile",
			req:  &api.UpdateNodePoolRequest{Nico: &api.NicoNodePoolProviderUpdate{OSProfile: ptrString("")}},
			want: "nico.osProfile cannot be cleared for a nico node pool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validateUpdateNodePoolRequest(forgeCluster(), tc.req)
			require.EqualError(t, err, tc.want)
		})
	}
}
