// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestValidateCreateNodePoolRequest_CAPICloudInitInputs(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
		Name:                       "worker",
		NodeNamePrefix:             "worker",
		ConfigProfile:              "default",
		DesiredNodeCount:           1,
		CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		CapiBootstrapFiles: []api.CloudInitFile{
			{Path: "/etc/custom.conf", Content: "hello"},
		},
		CapiPreKubeadmCommands: []string{"echo before kubeadm"},
	})
	require.NoError(t, err)
}

func TestValidateCreateNodePoolRequest_CAPIAllowsMissingConfigProfile(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
		Name:                       "worker",
		NodeNamePrefix:             "worker",
		DesiredNodeCount:           1,
		CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
	})
	require.NoError(t, err)
}

func TestValidateCreateNodePoolRequest_CAPIRejectsOSProfile(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
		Name:                       "worker",
		NodeNamePrefix:             "worker",
		OSProfile:                  "ubuntu",
		ConfigProfile:              "default",
		DesiredNodeCount:           1,
		CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
	})
	require.EqualError(t, err, "osProfile is not supported for CAPI clusters")
}

func TestValidateCreateNodePoolRequest_ForgeRejectsCAPICloudInitInputs(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.CreateNodePoolRequest{
		Name:                   "worker",
		NodeNamePrefix:         "worker",
		ConfigProfile:          "default",
		DesiredNodeCount:       1,
		CapiBootstrapFiles:     []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
		CapiPreKubeadmCommands: []string{"echo before kubeadm"},
	})
	require.EqualError(t, err, "capiBootstrapFiles is not supported for Forge clusters")
}

func TestValidateCreateNodePoolRequest_ForgeRequiresConfigProfile(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		InstanceTypeID:   uuid.New(),
		OSProfile:        "ubuntu",
		SubnetIDs:        []uuid.UUID{uuid.New()},
	})
	require.EqualError(t, err, "configProfile is required when creating a Forge node pool")
}

func TestValidateUpdatedNodePool_RejectsManagedSpirePathsWhenSpireEnabled(t *testing.T) {
	dbNodePool, err := dbNodePoolFromAPI(&api.CreateNodePoolRequest{
		ClusterID:                  uuid.New(),
		Name:                       "worker",
		NodeNamePrefix:             "worker",
		ConfigProfile:              "default",
		DesiredNodeCount:           1,
		CapiInfraMachineTemplate:   map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateTpm,
		CapiBootstrapFiles: []api.CloudInitFile{
			{Path: "/etc/spire-agent/agent.conf", Content: "override"},
		},
	}, "1.32.0")
	require.NoError(t, err)

	err = validateUpdatedNodePool(db.Cluster{SiteID: uuid.Nil}, &dbNodePool)
	require.EqualError(t, err, `file path "/etc/spire-agent/agent.conf" is managed by CWE and must not be provided`)
}

func TestDBNodePoolFromAPI_CopiesCAPICloudInitInputs(t *testing.T) {
	dbNodePool, err := dbNodePoolFromAPI(&api.CreateNodePoolRequest{
		ClusterID:                uuid.New(),
		Name:                     "worker",
		NodeNamePrefix:           "worker",
		ConfigProfile:            "default",
		DesiredNodeCount:         1,
		CapiInfraMachineTemplate: map[string]interface{}{"apiVersion": "infra.example.com/v1", "kind": "ExampleMachineTemplate"},
		CapiBootstrapFiles:       []api.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}},
		CapiPreKubeadmCommands:   []string{"echo before kubeadm"},
	}, "1.32.0")
	require.NoError(t, err)
	require.Equal(t, []db.CloudInitFile{{Path: "/etc/custom.conf", Content: "hello"}}, dbNodePool.CapiBootstrapFiles)
	require.Equal(t, []string{"echo before kubeadm"}, dbNodePool.CapiPreKubeadmCommands)
}
