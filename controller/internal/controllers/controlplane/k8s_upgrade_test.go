// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/clients"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestUpdateNodePoolKubeVersion_EmptyOSProfile(t *testing.T) {
	t.Parallel()

	nodePoolID := uuid.New()
	storedPool := db.NodePool{
		ID:                nodePoolID,
		KubernetesVersion: "1.34.2",
		Nico: &db.NicoNodePoolProvider{
			OSProfile: "",
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(storedPool, nil)
	mockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(pool *db.NodePool) bool {
		nico, err := pool.NicoProvider()
		return err == nil &&
			pool.ID == nodePoolID &&
			pool.KubernetesVersion == "1.35.0" &&
			nico.OSProfile == ""
	})).Return(db.NodePool{}, nil)

	r := &reconciler{db: mockTx, logger: slog.Default(), nodePoolUpgradeConfig: clients.GetNodePoolUpgradeConfig}
	err := r.updateNodePoolKubeVersion(
		context.Background(),
		db.Cluster{KubernetesVersion: "1.35.0", ProviderType: db.ProviderTypeCAPI},
		nodePool{NodePool: storedPool},
	)

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

// TestUpdateNodePoolKubeVersion_ResolveErrorSkipsUpdate exercises
// updateNodePoolKubeVersion's own error handling in isolation: a stubbed
// nodePoolUpgradeConfig forces ResolveNodePoolUpgrade to fail without going
// through real NICo/CAPI resolution, and the test confirms the error is
// returned and db.UpdateNodePool is never called.
func TestUpdateNodePoolKubeVersion_ResolveErrorSkipsUpdate(t *testing.T) {
	t.Parallel()

	nodePoolID := uuid.New()
	storedPool := db.NodePool{ID: nodePoolID, KubernetesVersion: "1.34.2"}
	resolveErr := errors.New("resolve boom")

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(storedPool, nil)

	r := &reconciler{
		db:     mockTx,
		logger: slog.Default(),
		nodePoolUpgradeConfig: func(db.Cluster) (clients.NodePoolUpgradeConfig, error) {
			return stubNodePoolUpgradeConfig{err: resolveErr}, nil
		},
	}
	err := r.updateNodePoolKubeVersion(
		context.Background(),
		db.Cluster{KubernetesVersion: "1.35.0"},
		nodePool{NodePool: storedPool},
	)

	require.ErrorIs(t, err, resolveErr)
	mockTx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
}

type stubNodePoolUpgradeConfig struct {
	err error
}

func (s stubNodePoolUpgradeConfig) ResolveNodePoolUpgrade(_ string, nodePool db.NodePool, _ string, _ *slog.Logger) (db.NodePool, error) {
	return nodePool, s.err
}
