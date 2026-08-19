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
	"go.temporal.io/sdk/testsuite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestSoftRebootNodeUsesTypedAgentRPC(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		ScheduleNodeRebootResponse: &nkeproto.ScheduleNodeRebootResponse{PackageLocksTimedOut: true},
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
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, "reboot", scriptName)
			require.Contains(t, scriptContents, "Rebooting the system now")
			return &nkeproto.ExecuteScriptResponse{}, nil
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

func TestGetBootTimeUsesTypedAgentResponse(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	want := time.Date(2026, time.August, 18, 14, 30, 45, 0, time.UTC)
	fakeAgent := &agenttest.FakeClient{
		GetNodeBootInfoResponse: &nkeproto.GetNodeBootInfoResponse{BootTimeUnixSeconds: want.Unix(), BootId: "boot-id"},
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
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, "uptime", scriptName)
			require.Contains(t, scriptContents, "Boot Time:")
			return &nkeproto.ExecuteScriptResponse{Output: "Node: worker-1\nBoot Time: 2026-08-18T14:30:45Z\nUptime: 0d 0h 1m 0s\n"}, nil
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
	fakeAgent := &agenttest.FakeClient{GetNodeBootInfoResponse: &nkeproto.GetNodeBootInfoResponse{}}
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
