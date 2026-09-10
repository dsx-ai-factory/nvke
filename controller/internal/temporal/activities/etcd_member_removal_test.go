// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
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

func TestRemoveEtcdMemberUsesSurvivingNode(t *testing.T) {
	clusterID := uuid.New()
	target := joinedControlPlaneNode(clusterID, "target", true)
	executor := joinedControlPlaneNode(clusterID, "executor", true)
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{target, executor}, nil).Once()
	fakeAgent := &agenttest.FakeClient{RemoveEtcdMemberResponse: &nkeproto.RemoveEtcdMemberResponse{}}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	err := executeEtcdRemovalActivity(t, a, EtcdMemberRemovalInput{
		ClusterID:        clusterID,
		TargetNodeID:     target.ID,
		TargetMemberName: target.Name,
	})

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{executor.ID}, fakeAgent.RemoveEtcdMemberCalls)
	require.Len(t, fakeAgent.RemoveEtcdMemberRequests, 1)
	assert.Equal(t, target.Name, fakeAgent.RemoveEtcdMemberRequests[0].GetTarget())
	database.AssertExpectations(t)
}

func TestRemoveEtcdMemberFallsBackOnlyOnUnimplemented(t *testing.T) {
	tests := []struct {
		name         string
		rpcError     error
		wantFallback bool
		wantError    string
	}{
		{name: "unimplemented falls back", rpcError: fmt.Errorf("wrapped: %w", status.Error(codes.Unimplemented, "unknown method")), wantFallback: true},
		{name: "operational failure propagates", rpcError: status.Error(codes.Internal, "etcd unavailable"), wantError: "etcd unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterID := uuid.New()
			target := joinedControlPlaneNode(clusterID, "target", true)
			executor := joinedControlPlaneNode(clusterID, "executor", true)
			database := &testutil.MockDB{}
			database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{target, executor}, nil).Once()
			if tt.wantFallback {
				database.On("GetNode", mock.Anything, executor.ID).Return(executor, nil).Once()
				database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster"}, nil).Once()
			}
			fakeAgent := &agenttest.FakeClient{
				RemoveEtcdMemberError: tt.rpcError,
				ExecuteScriptHook: func(_ db.Node, scriptName, _ string) (*nkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, legacyEtcdMemberRemoveRunbook, scriptName)
					return &nkeproto.ExecuteScriptResponse{}, nil
				},
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}

			err := executeEtcdRemovalActivity(t, a, EtcdMemberRemovalInput{
				ClusterID: clusterID, TargetNodeID: target.ID, TargetMemberName: target.Name,
			})

			if tt.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantError)
			}
			assert.Equal(t, tt.wantFallback, len(fakeAgent.ExecuteScriptNames) == 1)
			if tt.wantFallback {
				require.Equal(t, [][]string{{target.Name, "--allow-single-voter"}}, fakeAgent.ExecuteScriptArgs)
			}
			database.AssertExpectations(t)
		})
	}
}

func executeEtcdRemovalActivity(t *testing.T, a *Activities, input EtcdMemberRemovalInput) error {
	t.Helper()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.RemoveEtcdMember)
	_, err := env.ExecuteActivity(a.RemoveEtcdMember, input)
	return err
}
