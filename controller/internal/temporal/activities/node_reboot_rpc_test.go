// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestSoftRebootNodeUsesTypedAgentRPC(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		ScheduleNodeRebootResponse: &nvkeproto.ScheduleNodeRebootResponse{PackageLocksTimedOut: true},
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.softRebootNode)

	_, err := env.ExecuteActivity(activities.softRebootNode, SoftRebootNodeInput{NodeID: node.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.ScheduleNodeRebootCalls)
	database.AssertExpectations(t)
}

func TestSoftRebootNodeFallsBackToLegacyRunbookWhenRPCIsUnimplemented(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		ScheduleNodeRebootErr: fmt.Errorf("schedule reboot: %w", status.Error(codes.Unimplemented, "unknown method ScheduleNodeReboot")),
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nvkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, "reboot", scriptName)
			require.Contains(t, scriptContents, "Rebooting the system now")
			return &nvkeproto.ExecuteScriptResponse{}, nil
		},
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.softRebootNode)

	_, err := env.ExecuteActivity(activities.softRebootNode, SoftRebootNodeInput{NodeID: node.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.ScheduleNodeRebootCalls)
	require.Equal(t, []string{"reboot"}, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestSoftRebootNodeDoesNotFallBackForSupportedRPCFailure(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		ScheduleNodeRebootErr: status.Error(codes.Internal, "reboot scheduling failed"),
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.softRebootNode)

	_, err := env.ExecuteActivity(activities.softRebootNode, SoftRebootNodeInput{NodeID: node.ID})
	require.ErrorContains(t, err, "failed to schedule soft reboot")
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestSoftRebootNodeDefersUnknownRPCOutcomeToBootVerification(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()

			node := db.Node{ID: uuid.New(), Name: "worker-1"}
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				ScheduleNodeRebootErr: fmt.Errorf("schedule reboot: %w", status.Error(code, "connection lost")),
			}
			activities := &Activities{db: database, nodeAgentClient: fakeAgent}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.softRebootNode)

			_, err := env.ExecuteActivity(activities.softRebootNode, SoftRebootNodeInput{NodeID: node.ID})
			require.NoError(t, err)
			require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.ScheduleNodeRebootCalls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestSoftRebootNodeDefersUnknownLegacyRunbookOutcomeToBootVerification(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		ScheduleNodeRebootErr: status.Error(codes.Unimplemented, "unknown method ScheduleNodeReboot"),
		ExecuteScriptHook: func(_ db.Node, _, _ string) (*nvkeproto.ExecuteScriptResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection lost")
		},
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.softRebootNode)

	_, err := env.ExecuteActivity(activities.softRebootNode, SoftRebootNodeInput{NodeID: node.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.ScheduleNodeRebootCalls)
	require.Equal(t, []string{"reboot"}, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestSoftRebootNodeRetrySkipsDuplicateRequestAfterReboot(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	rebootInitiatedTime := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	fakeAgent := &agenttest.FakeClient{
		ScheduleNodeRebootErr: status.Error(codes.Internal, "response was not recorded"),
		GetNodeBootInfoResponse: &nvkeproto.GetNodeBootInfoResponse{
			BootTimeUnixSeconds: rebootInitiatedTime.Add(time.Minute).Unix(),
		},
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.RegisterActivity(activities.SoftRebootNode)

	env.ExecuteWorkflow(softRebootNodeRetryTestWorkflow, SoftRebootNodeInput{
		NodeID:              node.ID,
		RebootInitiatedTime: rebootInitiatedTime,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.ScheduleNodeRebootCalls)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.GetNodeBootInfoCalls)
	database.AssertExpectations(t)
}

func softRebootNodeRetryTestWorkflow(ctx workflow.Context, input SoftRebootNodeInput) error {
	var a *Activities
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Millisecond,
			MaximumAttempts: 2,
		},
	}

	return workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOptions), a.SoftRebootNode, input).Get(ctx, nil)
}

func TestGetBootTimeUsesTypedAgentResponse(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	want := time.Date(2026, time.August, 18, 14, 30, 45, 0, time.UTC)
	fakeAgent := &agenttest.FakeClient{
		GetNodeBootInfoResponse: &nvkeproto.GetNodeBootInfoResponse{BootTimeUnixSeconds: want.Unix(), BootId: "boot-id"},
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}

	got, err := activities.getBootTime(context.Background(), node.ID)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.GetNodeBootInfoCalls)
	database.AssertExpectations(t)
}

func TestGetBootTimeFallsBackToLegacyRunbookWhenRPCIsUnimplemented(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	want := time.Date(2026, time.August, 18, 14, 30, 45, 0, time.UTC)
	fakeAgent := &agenttest.FakeClient{
		GetNodeBootInfoErr: fmt.Errorf("get boot info: %w", status.Error(codes.Unimplemented, "unknown method GetNodeBootInfo")),
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nvkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, "uptime", scriptName)
			require.Contains(t, scriptContents, "Boot Time:")
			return &nvkeproto.ExecuteScriptResponse{Output: "Node: worker-1\nBoot Time: 2026-08-18T14:30:45Z\nUptime: 0d 0h 1m 0s\n"}, nil
		},
	}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.getBootTime)

	encoded, err := env.ExecuteActivity(activities.getBootTime, node.ID)
	require.NoError(t, err)
	var got time.Time
	require.NoError(t, encoded.Get(&got))
	require.Equal(t, want, got)
	require.Equal(t, []uuid.UUID{node.ID}, fakeAgent.GetNodeBootInfoCalls)
	require.Equal(t, []string{"uptime"}, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestGetBootTimeRejectsInvalidAgentResponse(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{GetNodeBootInfoResponse: &nvkeproto.GetNodeBootInfoResponse{}}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}

	_, err := activities.getBootTime(context.Background(), node.ID)
	require.ErrorContains(t, err, "returned an invalid boot time")
	database.AssertExpectations(t)
}

func TestGetBootTimeReturnsAgentError(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{GetNodeBootInfoErr: errors.New("agent unavailable")}
	activities := &Activities{db: database, nodeAgentClient: fakeAgent}

	_, err := activities.getBootTime(context.Background(), node.ID)
	require.ErrorContains(t, err, "failed to get boot info")
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}
