// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/controlplane"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestUnit_ControllerReconcileSkipsMissingCluster(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	mockTx := &testutil.MockTx{}
	mockDB := &testutil.MockDB{MockTx: mockTx}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)
	controller := controlplane.NewController(
		config.Config{},
		slog.Default(),
		dbwrapper.New(mockDB, slog.Default()),
		nil,
		nil,
		nil,
		nil,
	)

	result, err := controller.Reconcile(context.Background(), dbreconcile.ClusterKey(clusterID))

	require.NoError(t, err)
	assert.Empty(t, result)
	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}
