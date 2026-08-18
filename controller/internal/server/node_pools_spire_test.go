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

func TestAPINodePoolFromDB_SpireAttestationMethod(t *testing.T) {
	t.Run("omits spire attestation method when unset", func(t *testing.T) {
		apiNodePool := apiNodePoolFromDB(&db.NodePool{})
		require.Nil(t, apiNodePool.CapiSpireAttestationMethod)
	})

	t.Run("returns tpm attestation method when set", func(t *testing.T) {
		apiNodePool := apiNodePoolFromDB(&db.NodePool{
			CapiSpireAttestationMethod: db.SpireAttestationMethodTPM,
		})

		require.NotNil(t, apiNodePool.CapiSpireAttestationMethod)
		require.Equal(t, api.SpireAttestationMethodTpm, *apiNodePool.CapiSpireAttestationMethod)
	})

	t.Run("returns join token attestation method when set", func(t *testing.T) {
		apiNodePool := apiNodePoolFromDB(&db.NodePool{
			CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
		})

		require.NotNil(t, apiNodePool.CapiSpireAttestationMethod)
		require.Equal(t, api.SpireAttestationMethodJoinToken, *apiNodePool.CapiSpireAttestationMethod)
	})
}

func TestValidateCreateNodePoolRequest_SpireAttestationMethodRequiresCAPITemplate(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.New()}, &api.CreateNodePoolRequest{
		Name:                       "worker",
		NodeNamePrefix:             "worker",
		ConfigProfile:              "default",
		DesiredNodeCount:           1,
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateJoinToken,
	})
	require.EqualError(t, err, "capiSpireAttestationMethod is not supported for Forge clusters")
}

func TestValidateCreateNodePoolRequest_SpireAttestationMethodRequiredForCAPIClusters(t *testing.T) {
	service := NewNodePoolService(slog.Default(), nil, nil, config.TeleportConfig{}, false)

	err := service.validateCreateNodePoolRequest(context.Background(), db.Cluster{SiteID: uuid.Nil}, &api.CreateNodePoolRequest{
		Name:                     "worker",
		NodeNamePrefix:           "worker",
		ConfigProfile:            "default",
		DesiredNodeCount:         1,
		CapiInfraMachineTemplate: map[string]interface{}{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "ExampleMachineTemplate"},
	})
	require.EqualError(t, err, "capiSpireAttestationMethod is required for CAPI clusters")
}
