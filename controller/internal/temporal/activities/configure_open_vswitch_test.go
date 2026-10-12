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

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestConfigureOpenVSwitchUsesDurableTypedOperation(t *testing.T) {
	tests := map[string]nvkeproto.OpenVSwitchReconciliationResult_Outcome{
		"configured": nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED,
		"unchanged":  nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED,
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
				StartOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
					require.Equal(t, "op-1", req.GetOperationId())
					return successfulOpenVSwitchStart("op-1", outcome), nil
				},
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}

			result, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID, OperationID: "op-1"})

			require.NoError(t, err)
			assert.Equal(t, "rpc", result.Path)
			assert.Equal(t, "breth0", result.BridgeName)
			assert.Equal(t, "eth0", result.UplinkInterface)
			assert.Equal(t, "NETWORK_BACKEND_NETPLAN", result.NetworkBackend)
			assert.Equal(t, []uuid.UUID{nodeID}, fakeAgent.StartOpenVSwitchCalls)
			assert.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestConfigureOpenVSwitchPollsAcceptedOperationToSuccess(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		StartOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
			return &nvkeproto.StartOpenVSwitchReconciliationResponse{
				Operation: runningOpenVSwitchOperation(req.GetOperationId()),
			}, nil
		},
		GetOpenVSwitchStatusHook: func(_ db.Node, req *nvkeproto.GetOpenVSwitchReconciliationStatusRequest) (*nvkeproto.GetOpenVSwitchReconciliationStatusResponse, error) {
			return &nvkeproto.GetOpenVSwitchReconciliationStatusResponse{
				Operation: successfulOpenVSwitchOperation(req.GetOperationId(), nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED),
			}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	result, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID, OperationID: "op-1"})

	require.NoError(t, err)
	assert.Equal(t, "breth0", result.BridgeName)
	assert.Equal(t, []uuid.UUID{nodeID}, fakeAgent.GetOpenVSwitchStatusCalls)
	database.AssertExpectations(t)
}

func TestConfigureOpenVSwitchFallsBackOnlyWhenRPCIsUnimplemented(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Twice()
	fakeAgent := &agenttest.FakeClient{
		StartOpenVSwitchHook: func(_ db.Node, _ *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
			return nil, status.Error(codes.Unimplemented, "unknown method StartOpenVSwitchReconciliation")
		},
		ExecuteScriptHook: func(_ db.Node, scriptName, script string) (*nvkeproto.ExecuteScriptResponse, error) {
			require.Equal(t, legacyOpenVSwitchRunbook, scriptName)
			require.Contains(t, script, "configure_openvswitch")
			return &nvkeproto.ExecuteScriptResponse{}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ConfigureOpenVSwitch)

	value, err := env.ExecuteActivity(a.ConfigureOpenVSwitch, ConfigureOpenVSwitchInput{NodeID: nodeID, OperationID: "op-1"})

	require.NoError(t, err)
	var result ConfigureOpenVSwitchResult
	require.NoError(t, value.Get(&result))
	assert.Equal(t, "legacy_runbook", result.Path)
	assert.Equal(t, []uuid.UUID{nodeID}, fakeAgent.StartOpenVSwitchCalls)
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
	fakeAgent := &agenttest.FakeClient{
		StartOpenVSwitchHook: func(_ db.Node, _ *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
			return nil, errors.New("connection lost after accepting operation")
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}

	_, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID, OperationID: "op-1"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection lost after accepting operation")
	assert.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestConfigureOpenVSwitchRejectsInvalidOperation(t *testing.T) {
	for name, operation := range map[string]*nvkeproto.OpenVSwitchReconciliationOperation{
		"empty operation": nil,
		"missing status":  {},
		"unspecified phase": {
			Operation: &nvkeproto.NodeAgentOperationStatus{OperationId: "op-1"},
		},
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
					StartOpenVSwitchHook: func(_ db.Node, _ *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
						return &nvkeproto.StartOpenVSwitchReconciliationResponse{Operation: operation}, nil
					},
				},
			}

			_, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: nodeID, OperationID: "op-1"})

			require.Error(t, err)
			database.AssertExpectations(t)
		})
	}
}

func successfulOpenVSwitchStart(
	operationID string,
	outcome nvkeproto.OpenVSwitchReconciliationResult_Outcome,
) *nvkeproto.StartOpenVSwitchReconciliationResponse {
	return &nvkeproto.StartOpenVSwitchReconciliationResponse{
		Operation: successfulOpenVSwitchOperation(operationID, outcome),
	}
}

func runningOpenVSwitchOperation(operationID string) *nvkeproto.OpenVSwitchReconciliationOperation {
	return &nvkeproto.OpenVSwitchReconciliationOperation{
		Operation: &nvkeproto.NodeAgentOperationStatus{
			OperationId: operationID,
			Phase:       nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING,
		},
	}
}

func successfulOpenVSwitchOperation(
	operationID string,
	outcome nvkeproto.OpenVSwitchReconciliationResult_Outcome,
) *nvkeproto.OpenVSwitchReconciliationOperation {
	return &nvkeproto.OpenVSwitchReconciliationOperation{
		Operation: &nvkeproto.NodeAgentOperationStatus{
			OperationId: operationID,
			Phase:       nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
		},
		Result: &nvkeproto.OpenVSwitchReconciliationResult{
			Outcome:         outcome,
			BridgeName:      "breth0",
			UplinkInterface: "eth0",
			NetworkBackend:  nvkeproto.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN,
		},
	}
}
