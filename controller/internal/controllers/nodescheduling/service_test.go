// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodescheduling

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

func TestUnit_ServiceRetryAfter(t *testing.T) {
	require.Equal(t, 3*time.Minute, NewService(config.Config{
		NodeSchedulingRetryInterval: 3 * time.Minute,
	}, slog.Default(), nil, nil).RetryAfter())

	require.Equal(t, 2*time.Minute, NewService(config.Config{}, slog.Default(), nil, nil).RetryAfter())
}

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
		TemporalTaskQueue:           "test-task-queue",
		NodeRebootMaxTimeout:        7 * time.Minute,
		NodeSchedulingRetryInterval: time.Minute,
	}, slog.Default(), nil, temporalClient)

	result, err := service.ScheduleCAPIInfraNodes(ctx, slog.Default(), []db.Node{
		{ID: firstNodeID},
		{ID: secondNodeID},
	})
	require.NoError(t, err)
	require.Zero(t, result)
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

func (m mockWorkflowRun) Get(_ context.Context, _ interface{}) error {
	return nil
}
