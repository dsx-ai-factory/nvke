// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestAPINodePoolFromDB_SpireAttestationMethod(t *testing.T) {
	nodePoolWithCAPIMethod := func(method db.SpireAttestationMethod) *db.NodePool {
		pool := &db.NodePool{}
		pool.SetCapi(db.CAPINodePoolProvider{SpireAttestationMethod: method})
		return pool
	}

	t.Run("omits the infra payload for a pool that has none", func(t *testing.T) {
		apiNodePool := apiNodePoolFromDB(&db.NodePool{})
		require.Nil(t, apiNodePool.Infra)
	})

	t.Run("returns tpm attestation method when set", func(t *testing.T) {
		apiNodePool := apiNodePoolFromDB(nodePoolWithCAPIMethod(db.SpireAttestationMethodTPM))

		require.NotNil(t, apiNodePool.Infra)
		require.Equal(t, api.SpireAttestationMethodCreateTpm, apiNodePool.Infra.SpireAttestationMethod)
	})

	t.Run("returns join token attestation method when set", func(t *testing.T) {
		apiNodePool := apiNodePoolFromDB(nodePoolWithCAPIMethod(db.SpireAttestationMethodJoinToken))

		require.NotNil(t, apiNodePool.Infra)
		require.Equal(t, api.SpireAttestationMethodCreateJoinToken, apiNodePool.Infra.SpireAttestationMethod)
	})
}

func TestValidateCreateNodePoolRequest_SpireAttestationMethodRequiresCAPITemplate(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	err := service.validateCreateNodePoolRequest(capiCluster(), &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			SpireAttestationMethod: api.SpireAttestationMethodCreateJoinToken,
		},
	})
	require.EqualError(t, err, "infra.machineTemplate is required")
}

func TestValidateCreateNodePoolRequest_SpireAttestationMethodRequiredForCAPIClusters(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, config.TeleportConfig{}, nil, false)

	req := &api.CreateNodePoolRequest{
		Name:             "worker",
		NodeNamePrefix:   "worker",
		DesiredNodeCount: 1,
		Infra: &api.InfraNodePoolProvider{
			MachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
		},
	}
	err := service.validateCreateNodePoolRequest(capiCluster(), req)
	require.EqualError(t, err, "infra.spireAttestationMethod is required")
}
