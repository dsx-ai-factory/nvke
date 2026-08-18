// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

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
	database.AssertExpectations(t)
}
