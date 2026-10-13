// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodescheduling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

func TestUnit_ServiceScheduleCAPIInfraNodesStartsProvisionWorkflows(t *testing.T) {
	ctx := context.Background()
	firstNodeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	secondNodeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	temporalClient := &mockWorkflowStarter{}
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.TaskQueue == "test-task-queue" &&
			opts.ID == constants.ProvisionNodeWorkflowPrefix+firstNodeID.String() &&
			opts.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING &&
			opts.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == firstNodeID && input.RebootNodeWaitForUptimeTimeout == 7*time.Minute
	})).Return(mockWorkflowRun{id: "first"}, nil).Once()
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.TaskQueue == "test-task-queue" &&
			opts.ID == constants.ProvisionNodeWorkflowPrefix+secondNodeID.String()
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == secondNodeID && input.RebootNodeWaitForUptimeTimeout == 7*time.Minute
	})).Return(mockWorkflowRun{id: "second"}, nil).Once()

	service := NewService(config.Config{
		TemporalTaskQueue:    "test-task-queue",
		NodeRebootMaxTimeout: 7 * time.Minute,
	}, slog.Default(), nil, temporalClient)

	result, err := service.ScheduleCAPIInfraNodes(ctx, slog.Default(), []db.Node{
		{ID: firstNodeID},
		{ID: secondNodeID},
	})
	require.NoError(t, err)
	require.Zero(t, result)
	temporalClient.AssertExpectations(t)
}

func TestUnit_ServiceScheduleCAPIInfraNodesLogsWorkflowLookupFields(t *testing.T) {
	clusterID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	nodePoolID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	nodeID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	const (
		workflowID = "provision-node-eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		runID      = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	)

	temporalClient := &mockWorkflowStarter{}
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(mockWorkflowRun{id: workflowID, runID: runID}, nil).Once()
	var logs bytes.Buffer
	service := NewService(
		config.Config{TemporalTaskQueue: "test-task-queue"},
		slog.New(otelx.LogHandler{Handler: slog.NewJSONHandler(&logs, nil)}),
		nil,
		temporalClient,
	)

	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1},
		SpanID:  trace.SpanID{1},
	})
	ctx := trace.ContextWithSpanContext(t.Context(), spanContext)
	_, err := service.ScheduleCAPIInfraNodes(ctx, slog.Default(), []db.Node{{
		ID:         nodeID,
		NodePoolID: nodePoolID,
		ClusterID:  clusterID,
		Name:       "test-node",
	}})
	require.NoError(t, err)
	temporalClient.AssertExpectations(t)

	var record map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
	require.Equal(t, "Started (or already running) ProvisionNode workflow", record["msg"])
	require.Equal(t, clusterID.String(), record["cluster_id"])
	require.Equal(t, nodePoolID.String(), record["node_pool_id"])
	require.Equal(t, nodeID.String(), record["node_id"])
	require.Equal(t, "test-node", record["node_name"])
	require.Equal(t, workflowID, record["workflow_id"])
	require.Equal(t, runID, record["run_id"])
	require.Equal(t, spanContext.TraceID().String(), record["trace_id"])
	require.Equal(t, spanContext.SpanID().String(), record["span_id"])
}

func TestUnit_ServiceScheduleCAPIInfraNodesReportsNodeWhenWorkflowStartFails(t *testing.T) {
	nodeID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	temporalClient := &mockWorkflowStarter{}
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("Temporal unavailable")).Once()
	service := NewService(
		config.Config{TemporalTaskQueue: "test-task-queue"},
		slog.New(slog.DiscardHandler),
		nil,
		temporalClient,
	)

	_, err := service.ScheduleCAPIInfraNodes(t.Context(), slog.Default(), []db.Node{{ID: nodeID}})
	require.ErrorContains(t, err, "starting ProvisionNode workflow for node "+nodeID.String())
	require.ErrorContains(t, err, "Temporal unavailable")
	temporalClient.AssertExpectations(t)
}

type mockWorkflowStarter struct {
	mock.Mock
}

func (m *mockWorkflowStarter) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error) {
	callArgs := make([]interface{}, 0, 3+len(args))
	callArgs = append(callArgs, ctx, options, workflow)
	callArgs = append(callArgs, args...)
	ret := m.Called(callArgs...)
	run, _ := ret.Get(0).(client.WorkflowRun)
	return run, ret.Error(1)
}

type mockWorkflowRun struct {
	id    string
	runID string
}

func (m mockWorkflowRun) GetID() string {
	return m.id
}

func (m mockWorkflowRun) GetRunID() string {
	return m.runID
}

func (m mockWorkflowRun) GetFirstExecutionRunID() string {
	return m.runID
}

func (m mockWorkflowRun) Get(_ context.Context, _ interface{}) error {
	return nil
}

func (m mockWorkflowRun) GetWithOptions(_ context.Context, _ interface{}, _ client.WorkflowRunGetOptions) error {
	return nil
}
