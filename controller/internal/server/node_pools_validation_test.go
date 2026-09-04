// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/naming"
)

// A CAPI cluster rejects a nico payload whatever it carries: the provider shape
// is what is unsupported, so every field spelling reaches the same rejection.
func TestValidateCreateNodePoolRequest_CAPIRejectsNicoPayloadWhateverItCarries(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

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
			err := service.validateCreateNodePoolRequest(context.Background(), capiCluster(), &api.CreateNodePoolRequest{
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
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	cases := []struct {
		name  string
		infra *api.InfraNodePoolProvider
	}{
		{
			name:  "empty machineTemplate",
			infra: &api.InfraNodePoolProvider{MachineTemplate: map[string]interface{}{}},
		},
		{
			name:  "spireAttestationMethod",
			infra: &api.InfraNodePoolProvider{SpireAttestationMethod: api.SpireAttestationMethodCreateJoinToken},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := service.validateCreateNodePoolRequest(context.Background(), forgeCluster(), &api.CreateNodePoolRequest{
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

func TestValidateUpdateNodePoolRequest_CAPIRejectsForgeSpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	t.Run("nico provider shape", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil, ProviderType: db.ProviderTypeCAPI}, &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{SubnetIDs: []uuid.UUID{}},
		})

		require.EqualError(t, err, "nico is not supported, please use infra")
	})

	t.Run("empty infra.machineTemplate", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil, ProviderType: db.ProviderTypeCAPI}, &api.UpdateNodePoolRequest{
			Infra: &api.InfraNodePoolProviderUpdate{MachineTemplate: map[string]interface{}{}},
		})

		require.EqualError(t, err, "infra.machineTemplate cannot be empty")
	})

	t.Run("nico diskSelectionStrategy", func(t *testing.T) {
		largest := api.DiskSelectionStrategyLargest
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil, ProviderType: db.ProviderTypeCAPI}, &api.UpdateNodePoolRequest{
			Nico: &api.NicoNodePoolProviderUpdate{DiskSelectionStrategy: &largest},
		})

		require.EqualError(t, err, "nico is not supported, please use infra")
	})
}

func TestValidateUpdateNodePoolRequest_ForgeRejectsCAPISpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	t.Run("infra.bootstrapFiles", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New(), ProviderType: db.ProviderTypeNico}, &api.UpdateNodePoolRequest{
			Infra: &api.InfraNodePoolProviderUpdate{
				BootstrapFiles: []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
			},
		})

		require.EqualError(t, err, "infra is not supported, please use nico")
	})

	t.Run("infra.spireAttestationMethod", func(t *testing.T) {
		joinToken := api.SpireAttestationMethodJoinToken
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New(), ProviderType: db.ProviderTypeNico}, &api.UpdateNodePoolRequest{
			Infra: &api.InfraNodePoolProviderUpdate{
				SpireAttestationMethod: &joinToken,
			},
		})

		require.EqualError(t, err, "infra is not supported, please use nico")
	})
}

func TestValidateUpdatedNodePool_CAPIRequiresEmptyDiskSelectionStrategy(t *testing.T) {
	err := validateUpdatedNodePool(capiCluster(), &db.NodePool{
		CapiInfraMachineTemplate: []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ExampleMachineTemplate"}`),
		DiskSelectionStrategy:    db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateLargest),
	})

	require.EqualError(t, err, "nico.diskSelectionStrategy is not supported")
}

func TestValidateCreateNodePoolRequest_NodeNamePrefixLength(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)
	clusterName := "runtime-dgxc-k8s-forge-ytl-dev1"

	capiNodePoolRequest := func(nodeNamePrefix string) *api.CreateNodePoolRequest {
		return &api.CreateNodePoolRequest{
			Name:             "worker",
			NodeNamePrefix:   nodeNamePrefix,
			DesiredNodeCount: 1,
			Infra: &api.InfraNodePoolProvider{
				MachineTemplate:        map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
				SpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
			},
		}
	}
	cluster := capiCluster()
	cluster.Name = clusterName

	t.Run("accepts short prefix", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), cluster, capiNodePoolRequest("cp"))

		require.NoError(t, err)
	})

	t.Run("rejects prefix that exceeds generated node name limit", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(
			context.Background(),
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
			CniPlugin:                     "calico",
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
	err := svc.validateUpdateNodePoolRequest(context.Background(), capiCluster(), req)
	require.EqualError(t, err, "nico is not supported, please use infra")
}

// Clearing a profile is expressible in the request, and rejected: a Forge node
// pool always has one.
func TestValidateUpdateForgeNodePoolRequest_RejectsClearedProfiles(t *testing.T) {
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
		{
			name: "configProfile",
			req:  &api.UpdateNodePoolRequest{Nico: &api.NicoNodePoolProviderUpdate{ConfigProfile: ptrString("")}},
			want: "nico.configProfile cannot be cleared for a nico node pool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validateUpdateNodePoolRequest(context.Background(), forgeCluster(), tc.req)
			require.EqualError(t, err, tc.want)
		})
	}
}
