// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/naming"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// A CAPI cluster rejects a nico payload whatever it carries: the provider shape
// is what is unsupported, so every field spelling reaches the same rejection.
func TestValidateCreateNodePoolRequest_CAPIRejectsNicoPayloadWhateverItCarries(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

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
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

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
					Release:        "ubuntu",
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
		service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderNico})

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
		service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderInfra})

		err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
			Name:             "worker",
			NodeNamePrefix:   "worker",
			DesiredNodeCount: 1,
			Nico: &api.NicoNodePoolProvider{
				SubnetIDs:      []uuid.UUID{uuid.New()},
				InstanceTypeID: uuid.New(),
				Release:        "ubuntu",
				ConfigProfile:  "default",
			},
		})

		require.EqualError(t, err, "nico provider is not supported by this deployment")
	})
}

func TestValidateCreateNodePoolRequest_LaunchLayer(t *testing.T) {
	cluster := launchLayerCluster()
	cluster.LaunchLayer.SecurityGroups = []db.LaunchLayerSecurityGroup{{Name: "workers", Type: "BARE_METAL"}}

	tests := map[string]struct {
		nodeAgentProfile db.Node
		refs             *api.LaunchLayerSecurityGroupRefs
		vmProfile        string
		wantErr          string
	}{
		"accepts securityGroupRefs naming the cluster's groups": {
			refs: &api.LaunchLayerSecurityGroupRefs{PrimaryNetworkInterface: []string{"workers"}},
		},
		"rejects a securityGroupRefs name the cluster does not define": {
			refs:    &api.LaunchLayerSecurityGroupRefs{GpuNetworkInterface: []string{"missing"}},
			wantErr: `launchLayer.securityGroupRefs: "missing" is not one of the cluster's launchLayer.securityGroups`,
		},
		"rejects a pool whose nodes could never join": {
			nodeAgentProfile: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			wantErr: "this deployment's node-agent settings do not support the launchlayer provider: " +
				"SPIFFE-enabled LaunchLayer nodes require dsxImds SPIRE attestation, got tpm",
		},
		"rejects a vmProfile pool whose nodes could never attest": {
			nodeAgentProfile: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS},
			vmProfile:        "ubuntu",
			wantErr: "this deployment's node-agent settings do not support the launchlayer provider: " +
				`SPIFFE-enabled LaunchLayer nodes without external mTLS require hostOSProfile: dsxImds attestation is not available to vmProfile "ubuntu" instances`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderLaunchLayer})
			service.nodeAgentProfile = tt.nodeAgentProfile

			err := service.validateCreateNodePoolRequest(cluster, &api.CreateNodePoolRequest{
				Name:             "worker",
				NodeNamePrefix:   "worker",
				DesiredNodeCount: 1,
				LaunchLayer:      &api.LaunchLayerNodePoolProvider{InstanceType: "dgx200", VMProfile: tt.vmProfile, SecurityGroupRefs: tt.refs},
			})
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateCreateNodePoolRequest_DisabledProviderPrecedesProviderShape(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderNico})

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
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, []string{config.InfraProviderNico})
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
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

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

	t.Run("launchLayer provider shape", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeCAPI}, &api.UpdateNodePoolRequest{
			LaunchLayer: &api.LaunchLayerNodePoolProviderUpdate{},
		})

		require.EqualError(t, err, "launchLayer is not supported, please use infra")
	})
}

func TestValidateUpdateNodePoolRequest_ForgeRejectsCAPISpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	t.Run("infra.bootstrapFiles", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeNico, Nico: &db.NicoClusterProvider{SiteID: uuid.New()}}, &api.UpdateNodePoolRequest{
			Infra: &api.InfraNodePoolProviderUpdate{
				BootstrapFiles: []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
			},
		})

		require.EqualError(t, err, "infra is not supported, please use nico")
	})

	t.Run("launchLayer provider shape", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(db.Cluster{ProviderType: db.ProviderTypeNico, Nico: &db.NicoClusterProvider{SiteID: uuid.New()}}, &api.UpdateNodePoolRequest{
			LaunchLayer: &api.LaunchLayerNodePoolProviderUpdate{},
		})

		require.EqualError(t, err, "launchLayer is not supported, please use nico")
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
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
		Name:           "cp",
		NodeNamePrefix: "cp",
		ControlPlane:   true,
		NodeTaints: []api.NodeTaint{{
			Key:    "dedicated",
			Effect: api.NodeTaintEffectNoSchedule,
		}},
	})

	require.EqualError(t, err, "nodeTaints: not allowed on a control-plane pool")
}

func TestValidateCreateNodePoolRequest_RejectsReservedLabel(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)

	err := service.validateCreateNodePoolRequest(forgeCluster(), &api.CreateNodePoolRequest{
		Name:           "workers",
		NodeNamePrefix: "w",
		NodeLabels:     map[string]string{"nke.nvidia.com/managed": "true"},
	})

	require.EqualError(t, err, `nodeLabels: key "nke.nvidia.com/managed" is under reserved domain "nke.nvidia.com"; labels in this domain cannot be set`)
}

func TestValidateCreateNodePoolRequest_UsesConfiguredLabelAllowlist(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)
	service.allowlistedLabelKeys = []string{"node.kubernetes.io/custom"}

	req := &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "w",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		},
		NodeLabels: map[string]string{"node.kubernetes.io/custom": "true"},
	}
	require.NoError(t, service.validateCreateNodePoolRequest(capiCluster(), req))

	req.NodeLabels = map[string]string{"node.kubernetes.io/instance-type": "h100"}
	require.EqualError(t, service.validateCreateNodePoolRequest(capiCluster(), req),
		`nodeLabels: key "node.kubernetes.io/instance-type" is under reserved domain "kubernetes.io"; labels in this domain cannot be set`)
}

func TestValidateCreateNodePoolRequest_NodeNamePrefixLength(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil)
	clusterName := "runtime-dgxc-k8s-forge-site-2-dev1"

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
			capiNodePoolRequest("runtime-dgxc-k8s-forge-site-2-dev1-cp"),
		)

		require.Error(t, err)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestValidateCreateClusterRequest_NodeNamePrefixLength(t *testing.T) {
	clusterName := "runtime-dgxc-k8s-forge-site-2-dev1"
	svc := clusterService{}

	forgeClusterRequest := func(name, nodeNamePrefix string) *api.CreateClusterRequest {
		nodePool := validForgeNodePoolCreateConfig("w")
		nodePool.NodeNamePrefix = nodeNamePrefix
		controlPlaneConfig := validForgeControlPlaneConfig()
		controlPlaneConfig.DesiredCount = 1
		return &api.CreateClusterRequest{
			Name: name,
			Nico: &api.NicoClusterProvider{
				OrgID:  "test-org",
				SiteID: uuid.New(),
				VpcID:  uuid.New(),
			},
			CniPlugin:                     "ovn-kubernetes",
			ControlPlaneKubernetesVersion: "1.29.0",
			ControlPlaneConfig:            controlPlaneConfig,
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
			forgeClusterRequest(clusterName, "runtime-dgxc-k8s-forge-site-2-dev1-cp"),
		)

		require.Error(t, err)
		require.Contains(t, err.Error(), `invalid node pool "w"`)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestValidateUpdateNodePoolRequest_WrongProvider(t *testing.T) {
	svc := &nodePoolService{}
	req := &api.UpdateNodePoolRequest{
		Nico: &api.NicoNodePoolProviderUpdate{Release: ptrString("os")},
	}
	err := svc.validateUpdateNodePoolRequest(capiCluster(), req)
	require.EqualError(t, err, "nico is not supported, please use infra")
}

// Clearing the release is expressible in the request, and rejected: a Forge
// node pool always has one. The config profile is optional and may be cleared.
func TestValidateUpdateForgeNodePoolRequest_RejectsClearedRelease(t *testing.T) {
	svc := &nodePoolService{}
	cases := []struct {
		name string
		req  *api.UpdateNodePoolRequest
		want string
	}{
		{
			name: "release",
			req:  &api.UpdateNodePoolRequest{Nico: &api.NicoNodePoolProviderUpdate{Release: ptrString("")}},
			want: "nico.release cannot be cleared for a nico node pool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validateUpdateNodePoolRequest(forgeCluster(), tc.req)
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestUpdateNodePool_ScaleUpChecksNodeAgentProfile(t *testing.T) {
	cluster := launchLayerCluster()
	cluster.ID = uuid.New()
	nodePool := db.NodePool{ID: uuid.New(), ClusterID: cluster.ID, DesiredCount: 2}
	nodePool.SetLaunchLayer(db.LaunchLayerNodePoolProvider{InstanceType: "vm200", VMProfile: "ubuntu"})

	tests := map[string]struct {
		desiredNodeCount int
		want400          bool
	}{
		"scale-up of a vmProfile pool is rejected": {desiredNodeCount: 3, want400: true},
		"scale-down is not checked":                {desiredNodeCount: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tx := &testutil.MockTx{}
			tx.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
			tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil)
			tx.On("GetNodePoolForUpdate", mock.Anything, nodePool.ID).Return(nodePool, nil)
			tx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{nodePool}, nil)
			tx.On("UpdateNodePool", mock.Anything, mock.Anything).Maybe().Return(nodePool, nil)
			database := &testutil.MockDB{MockTx: tx}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			service := NewNodePoolService(slog.Default(), database, config.TeleportConfig{}, []string{config.InfraProviderLaunchLayer})
			service.nodeAgentProfile = db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS}

			resp, err := service.UpdateNodePool(t.Context(), api.UpdateNodePoolRequestObject{
				NodePoolID: nodePool.ID,
				Body:       &api.UpdateNodePoolRequest{DesiredNodeCount: &tt.desiredNodeCount},
			})

			require.NoError(t, err)
			if !tt.want400 {
				require.IsType(t, api.UpdateNodePool200JSONResponse{}, resp)
				return
			}
			badRequest, ok := resp.(api.UpdateNodePool400JSONResponse)
			require.True(t, ok, "expected UpdateNodePool400JSONResponse, got %T", resp)
			require.Contains(t, badRequest.Message, `dsxImds attestation is not available to vmProfile "ubuntu" instances`)
			tx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
		})
	}
}
