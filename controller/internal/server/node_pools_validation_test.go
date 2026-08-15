// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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

func TestValidateCreateNodePoolRequest_CAPIRejectsForgeSpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	t.Run("instanceTypeID", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
			Name:                       "worker",
			NodeNamePrefix:             "worker",
			ConfigProfile:              "default",
			DesiredNodeCount:           1,
			InstanceTypeID:             uuid.New(),
			CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		})

		require.EqualError(t, err, "instanceTypeID is not supported for CAPI clusters")
	})

	t.Run("explicit empty subnetIDs", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
			Name:                       "worker",
			NodeNamePrefix:             "worker",
			ConfigProfile:              "default",
			DesiredNodeCount:           1,
			SubnetIDs:                  []uuid.UUID{},
			CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		})

		require.EqualError(t, err, "subnetIDs is not supported for CAPI clusters")
	})

	t.Run("diskSelectionStrategy", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
			Name:                       "worker",
			NodeNamePrefix:             "worker",
			ConfigProfile:              "default",
			DesiredNodeCount:           1,
			DiskSelectionStrategy:      api.DiskSelectionStrategyCreateLargest,
			CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		})

		require.EqualError(t, err, "diskSelectionStrategy is not supported for CAPI clusters")
	})
}

func TestValidateCreateNodePoolRequest_ForgeRejectsCAPISpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	t.Run("empty capiInfraMachineTemplate", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.CreateNodePoolRequest{
			Name:                     "worker",
			NodeNamePrefix:           "worker",
			ConfigProfile:            "default",
			DesiredNodeCount:         1,
			CapiInfraMachineTemplate: map[string]interface{}{},
		})

		require.EqualError(t, err, "capiInfraMachineTemplate is not supported for Forge clusters")
	})

	t.Run("capiSpireAttestationMethod", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.CreateNodePoolRequest{
			Name:                       "worker",
			NodeNamePrefix:             "worker",
			ConfigProfile:              "default",
			DesiredNodeCount:           1,
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateJoinToken,
		})

		require.EqualError(t, err, "capiSpireAttestationMethod is not supported for Forge clusters")
	})
}

func TestValidateUpdateNodePoolRequest_CAPIRejectsForgeSpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	t.Run("explicit empty subnetIDs", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.UpdateNodePoolRequest{
			SubnetIDs: []uuid.UUID{},
		})

		require.EqualError(t, err, "subnetIDs is not supported for CAPI clusters")
	})

	t.Run("empty capiInfraMachineTemplate", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.UpdateNodePoolRequest{
			CapiInfraMachineTemplate: &map[string]interface{}{},
		})

		require.EqualError(t, err, "capiInfraMachineTemplate cannot be empty for CAPI clusters")
	})

	t.Run("diskSelectionStrategy", func(t *testing.T) {
		strategy := api.DiskSelectionStrategyLargest
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.UpdateNodePoolRequest{
			DiskSelectionStrategy: &strategy,
		})

		require.EqualError(t, err, "diskSelectionStrategy is not supported for CAPI clusters")
	})
}

func TestValidateUpdateNodePoolRequest_ForgeRejectsCAPISpecificFieldsIndividually(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	t.Run("capiBootstrapFiles", func(t *testing.T) {
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.UpdateNodePoolRequest{
			CapiBootstrapFiles: []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
		})

		require.EqualError(t, err, "capiBootstrapFiles is not supported for Forge clusters")
	})

	t.Run("capiSpireAttestationMethod", func(t *testing.T) {
		method := api.SpireAttestationMethodJoinToken
		err := service.validateUpdateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.UpdateNodePoolRequest{
			CapiSpireAttestationMethod: &method,
		})

		require.EqualError(t, err, "capiSpireAttestationMethod is not supported for Forge clusters")
	})
}

func TestValidateUpdatedNodePool_CAPIRequiresEmptyDiskSelectionStrategy(t *testing.T) {
	err := validateUpdatedNodePool(db.Cluster{SiteID: uuid.Nil}, &db.NodePool{
		CapiInfraMachineTemplate: []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ExampleMachineTemplate"}`),
		DiskSelectionStrategy:    db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateLargest),
	})

	require.EqualError(t, err, "diskSelectionStrategy is not supported for CAPI clusters")
}

func TestValidateCreateNodePoolRequest_NodeNamePrefixLength(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)
	clusterName := "runtime-dgxc-k8s-forge-ytl-dev1"

	t.Run("accepts short prefix", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{
			Name:   clusterName,
			SiteID: uuid.Nil,
		}, &api.CreateNodePoolRequest{
			Name:                       "worker",
			NodeNamePrefix:             "cp",
			ConfigProfile:              "default",
			DesiredNodeCount:           1,
			CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		})

		require.NoError(t, err)
	})

	t.Run("rejects prefix that exceeds generated node name limit", func(t *testing.T) {
		err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{
			Name:   clusterName,
			SiteID: uuid.Nil,
		}, &api.CreateNodePoolRequest{
			Name:                       "worker",
			NodeNamePrefix:             "runtime-dgxc-k8s-forge-ytl-dev1-cp",
			ConfigProfile:              "default",
			DesiredNodeCount:           1,
			CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		})

		require.Error(t, err)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestValidateCreateClusterRequest_NodeNamePrefixLength(t *testing.T) {
	clusterName := "runtime-dgxc-k8s-forge-ytl-dev1"
	svc := clusterService{}

	t.Run("rejects cluster name too long for generated node names", func(t *testing.T) {
		err := svc.validateCreateClusterRequestFields(&api.CreateClusterRequest{
			Name:                          strings.Repeat("a", naming.MaxGeneratedNodeNameLength),
			OrgID:                         "test-org",
			SiteID:                        uuid.New(),
			VpcID:                         uuid.New(),
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			NodePools: &[]api.NodePoolCreateConfig{
				{
					Name:             "cp",
					NodeNamePrefix:   "cp",
					SubnetIDs:        []uuid.UUID{uuid.New()},
					InstanceTypeID:   uuid.New(),
					OSProfile:        "ubuntu",
					ConfigProfile:    "default",
					ControlPlane:     true,
					DesiredNodeCount: 1,
				},
			},
		})

		require.Error(t, err)
		require.Contains(t, err.Error(), "cluster name")
		require.Contains(t, err.Error(), "too long")
		require.Contains(t, err.Error(), naming.GeneratedNodeNameFormat)
		require.NotContains(t, err.Error(), `invalid node pool`)
	})

	t.Run("rejects embedded node pool with prefix too long for cluster name", func(t *testing.T) {
		err := svc.validateCreateClusterRequestFields(&api.CreateClusterRequest{
			Name:                          clusterName,
			OrgID:                         "test-org",
			SiteID:                        uuid.New(),
			VpcID:                         uuid.New(),
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.29.0",
			NodePools: &[]api.NodePoolCreateConfig{
				{
					Name:             "cp",
					NodeNamePrefix:   "runtime-dgxc-k8s-forge-ytl-dev1-cp",
					SubnetIDs:        []uuid.UUID{uuid.New()},
					InstanceTypeID:   uuid.New(),
					OSProfile:        "ubuntu",
					ConfigProfile:    "default",
					ControlPlane:     true,
					DesiredNodeCount: 1,
				},
			},
		})

		require.Error(t, err)
		require.Contains(t, err.Error(), `invalid node pool "cp"`)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})
}
