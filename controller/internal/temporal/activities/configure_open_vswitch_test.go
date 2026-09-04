// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
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

func TestConfigureOpenVSwitchUsesTypedRPC(t *testing.T) {
	tests := map[string]nkeproto.ReconcileOpenVSwitchResponse_Outcome{
		"configured": nkeproto.ReconcileOpenVSwitchResponse_OUTCOME_CONFIGURED,
		"unchanged":  nkeproto.ReconcileOpenVSwitchResponse_OUTCOME_UNCHANGED,
	}
	for name, outcome := range tests {
		t.Run(name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
			database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				ReconcileOpenVSwitchResponse: &nkeproto.ReconcileOpenVSwitchResponse{
					Outcome:         outcome,
					BridgeName:      "breth0",
					UplinkInterface: "eth0",
					NetworkBackend:  nkeproto.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_NETPLAN,
				},
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}

			result, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID})

			require.NoError(t, err)
			assert.Equal(t, "rpc", result.Path)
			assert.Equal(t, "breth0", result.BridgeName)
			assert.Equal(t, "eth0", result.UplinkInterface)
			assert.Equal(t, "NETWORK_BACKEND_NETPLAN", result.NetworkBackend)
			assert.Equal(t, []uuid.UUID{nodeID}, fakeAgent.ReconcileOpenVSwitchCalls)
			require.Len(t, fakeAgent.ReconcileOpenVSwitchRequests, 1)
			assert.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestConfigureOpenVSwitchFallsBackOnlyWhenRPCIsUnimplemented(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Twice()
	fakeAgent := &agenttest.FakeClient{
		ReconcileOpenVSwitchErr: status.Error(codes.Unimplemented, "unknown method ReconcileOpenVSwitch"),
		ExecuteScriptHook: func(_ db.Node, scriptName, script string) (*nkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, legacyOpenVSwitchRunbook, scriptName)
			require.Contains(t, script, "configure_openvswitch")
			return &nkeproto.ExecuteScriptResponse{}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ConfigureOpenVSwitch)

	value, err := env.ExecuteActivity(a.ConfigureOpenVSwitch, ConfigureOpenVSwitchInput{NodeID: nodeID})

	require.NoError(t, err)
	var result ConfigureOpenVSwitchResult
	require.NoError(t, value.Get(&result))
	assert.Equal(t, "legacy_runbook", result.Path)
	assert.Equal(t, []uuid.UUID{nodeID}, fakeAgent.ReconcileOpenVSwitchCalls)
	assert.Equal(t, []string{legacyOpenVSwitchRunbook}, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestConfigureOpenVSwitchPropagatesOperationalRPCErrorWithoutFallback(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{ReconcileOpenVSwitchErr: errors.New("connection lost during netplan apply")}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	_, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection lost during netplan apply")
	assert.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestConfigureOpenVSwitchRejectsInvalidRPCResponse(t *testing.T) {
	for name, response := range map[string]*nkeproto.ReconcileOpenVSwitchResponse{
		"empty response":      nil,
		"unspecified outcome": {},
	} {
		t.Run(name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
			database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
			a := &Activities{
				db: database,
				nodeAgentClient: &agenttest.FakeClient{
					ReconcileOpenVSwitchResponse: response,
				},
			}

			_, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID})

			require.Error(t, err)
			database.AssertExpectations(t)
		})
	}
}
