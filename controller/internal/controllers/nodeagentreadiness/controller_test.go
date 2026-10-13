// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
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
	require.Equal(t, 2*time.Minute, result.RequeueAfter)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

type blockingAgent struct {
	agent.Client
	started chan struct{}
}

func (a *blockingAgent) GetAgentVersion(ctx context.Context, _ db.Node) (string, error) {
	if a.started != nil {
		close(a.started)
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func TestUnit_AgentProbeUsesDedicatedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const probeTimeout = 5 * time.Second
		nodeID := uuid.New()
		node := db.Node{ID: nodeID, Conditions: db.NodeConditions{
			NodeAgentReady: db.NewCondition(false, context.DeadlineExceeded.Error()),
		}}
		database := &testutil.MockDB{}
		database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
		c := NewAgentReadinessController(config.Config{
			NodeHealthCheckTimeout: time.Minute,
			AgentReadiness:         config.AgentReadinessConfig{Timeout: probeTimeout},
		}, slog.New(slog.DiscardHandler), dbwrapper.New(database, nil), &blockingAgent{})
		started := time.Now()
		result, err := c.Reconcile(t.Context(), dbreconcile.NodeKey(nodeID))
		require.NoError(t, err)
		require.Equal(t, probeTimeout, time.Since(started))
		require.Equal(t, 2*time.Minute, result.RequeueAfter)
		database.AssertExpectations(t)
		database.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	})
}

func TestUnit_AgentProbeShutdownDoesNotChangeReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodeID := uuid.New()
		database := &testutil.MockDB{}
		database.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID}, nil).Once()
		probeStarted := make(chan struct{})
		c := NewAgentReadinessController(config.Config{}, slog.New(slog.DiscardHandler), dbwrapper.New(database, nil), &blockingAgent{started: probeStarted})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := c.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
			done <- err
		}()
		<-probeStarted
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Zero(t, time.Since(started))
		database.AssertExpectations(t)
		database.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	})
}

func TestUnit_AgentProbeJitterBounds(t *testing.T) {
	c := NewAgentReadinessController(config.Config{AgentReadiness: config.AgentReadinessConfig{
		Interval: 2 * time.Minute, Jitter: 0.1,
	}}, nil, nil, nil)
	var total time.Duration
	for range 1000 {
		delay := c.requeueAfter()
		require.GreaterOrEqual(t, delay, 108*time.Second)
		require.Less(t, delay, 132*time.Second)
		total += delay
	}
	require.InDelta(t, 120, (total / 1000).Seconds(), 2)
}

func TestUnit_AgentProbeTimeoutMarksReadyNodeUnready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodeID := uuid.New()
		node := db.Node{ID: nodeID, AgentVersion: "v9.9.9", Conditions: db.NodeConditions{
			NodeAgentReady: db.NewCondition(true, "Node agent is reachable"),
		}}
		database := &testutil.MockDB{}
		database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
		database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
		tx := &testutil.MockTx{}
		database.MockTx = tx
		tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(node, nil).Once()
		tx.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
		tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
			return updated.Conditions.NodeAgentReady.Status == db.ConditionStatusFalse &&
				updated.Conditions.NodeAgentReady.Message == context.DeadlineExceeded.Error() &&
				updated.AgentVersion == node.AgentVersion
		})).Return(node, nil).Once()
		c := NewAgentReadinessController(config.Config{AgentReadiness: config.AgentReadinessConfig{
			Timeout: 5 * time.Second,
		}}, slog.New(slog.DiscardHandler), dbwrapper.New(database, nil), &blockingAgent{})

		result, err := c.Reconcile(t.Context(), dbreconcile.NodeKey(nodeID))
		require.NoError(t, err)
		require.Equal(t, 2*time.Minute, result.RequeueAfter)
		database.AssertExpectations(t)
		tx.AssertExpectations(t)
	})
}
