// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestUnit_AgentReadinessControllerRequeuesAfterProbe(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.New()
	node := db.Node{ID: nodeID, Name: "worker-1"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()

	tx := &testutil.MockTx{}
	database.MockTx = tx
	tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(node, nil).Once()
	tx.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		return updated.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue &&
			updated.AgentVersion == "v9.9.9"
	})).Return(node, nil).Once()

	controller := NewAgentReadinessController(
		config.Config{NodeHealthCheckTimeout: time.Second},
		slog.New(slog.DiscardHandler),
		dbwrapper.New(database, slog.New(slog.DiscardHandler)),
		&agenttest.FakeClient{AgentVersion: "v9.9.9"},
	)

	result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)
	require.Equal(t, agentReadinessResyncInterval, result.RequeueAfter)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}
