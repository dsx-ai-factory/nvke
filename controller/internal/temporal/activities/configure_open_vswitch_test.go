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
	"go.temporal.io/sdk/temporal"
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
			node := db.Node{ID: nodeID, NodeSetID: nodeID, NodePoolID: nodeID, ClusterID: clusterID, Name: "node-1"}
			database := &testutil.MockDB{}
			mockOpenVSwitchNodeSet(database, node, nil)
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
	node := db.Node{ID: nodeID, NodeSetID: nodeID, NodePoolID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	mockOpenVSwitchNodeSet(database, node, nil)
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
	node := db.Node{ID: nodeID, NodeSetID: nodeID, NodePoolID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	mockOpenVSwitchNodeSet(database, node, nil)
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
	node := db.Node{ID: nodeID, NodeSetID: nodeID, NodePoolID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	mockOpenVSwitchNodeSet(database, node, nil)
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
			node := db.Node{ID: nodeID, NodeSetID: nodeID, NodePoolID: nodeID, ClusterID: clusterID, Name: "node-1"}
			database := &testutil.MockDB{}
			mockOpenVSwitchNodeSet(database, node, nil)
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

func mockOpenVSwitchNodeSet(database *testutil.MockDB, node db.Node, desired *db.OpenVSwitchConfig) {
	database.On("GetNodeSet", mock.Anything, node.NodeSetID).Return(db.NodeSet{
		ID: node.NodeSetID, NodePoolID: node.NodePoolID, OpenVSwitch: desired,
	}, nil).Once()
	database.On("GetNodePool", mock.Anything, node.NodePoolID).Return(db.NodePool{ID: node.NodePoolID}, nil).Once()
}

func TestConfigureOpenVSwitchUsesPinnedNodeSet(t *testing.T) {
	node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), NodeSetID: uuid.New(), NodePoolID: uuid.New()}
	desired := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID}, nil).Once()
	database.On("GetNodeSet", mock.Anything, node.NodeSetID).Return(db.NodeSet{
		ID: node.NodeSetID, NodePoolID: node.NodePoolID, OpenVSwitch: desired,
	}, nil).Once()
	database.On("GetNodePool", mock.Anything, node.NodePoolID).Return(db.NodePool{
		ID: node.NodePoolID, OpenVSwitch: &db.OpenVSwitchConfig{UplinkInterface: "new0", BridgeName: "brnew0"},
	}, nil).Once()
	fakeAgent := configuredOpenVSwitchFake(t, desired)
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	result, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"})
	require.NoError(t, err)
	require.Equal(t, "br-eth1", result.BridgeName)
	require.Empty(t, fakeAgent.StartOpenVSwitchCalls)
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestConfigureOpenVSwitchUsesPinnedControlPlaneConfiguration(t *testing.T) {
	node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), ControlPlane: true, ControlPlaneConfigurationID: uuid.New()}
	desired := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	cluster := db.Cluster{ID: node.ClusterID, ProviderType: db.ProviderTypeNico}
	cluster.ControlPlaneConfig.OpenVSwitch = &db.OpenVSwitchConfig{UplinkInterface: "new0", BridgeName: "brnew0"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(cluster, nil).Twice()
	database.On("GetControlPlaneConfiguration", mock.Anything, node.ControlPlaneConfigurationID).Return(db.ControlPlaneConfiguration{
		Config: db.ControlPlaneConfigSpec{OpenVSwitch: desired},
	}, nil).Once()
	fakeAgent := configuredOpenVSwitchFake(t, desired)
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	result, err := a.ConfigureOpenVSwitch(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"})
	require.NoError(t, err)
	require.Equal(t, "br-eth1", result.BridgeName)
	require.Empty(t, fakeAgent.StartOpenVSwitchCalls)
	database.AssertExpectations(t)
}

func configuredOpenVSwitchFake(t *testing.T, desired *db.OpenVSwitchConfig) *agenttest.FakeClient {
	t.Helper()
	return &agenttest.FakeClient{
		StartConfiguredOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartConfiguredOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
			require.Equal(t, desired.UplinkInterface, req.GetUplinkInterface())
			require.Equal(t, desired.BridgeName, req.GetBridgeName())
			response := successfulOpenVSwitchStart(req.GetOperationId(), nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED)
			response.Operation.Result.UplinkInterface = desired.UplinkInterface
			response.Operation.Result.BridgeName = desired.BridgeName
			return response, nil
		},
	}
}

func TestConfigureOpenVSwitchRejectsUnpinnedExplicitControlPlaneIntent(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "legacy default-route intent"
		if explicit {
			name = "explicit intent requires a snapshot"
		}
		t.Run(name, func(t *testing.T) {
			node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), ControlPlane: true, Name: "cp-1"}
			cluster := db.Cluster{ID: node.ClusterID, ProviderType: db.ProviderTypeNico}
			if explicit {
				cluster.ControlPlaneConfig.OpenVSwitch = &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
			}
			database := &testutil.MockDB{}
			database.On("GetCluster", mock.Anything, node.ClusterID).Return(cluster, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				StartOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
					return successfulOpenVSwitchStart(req.GetOperationId(), nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED), nil
				},
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			_, _, err := a.configureOpenVSwitchOnNode(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"}, node)
			if explicit {
				var applicationError *temporal.ApplicationError
				require.ErrorAs(t, err, &applicationError)
				require.True(t, applicationError.NonRetryable())
				require.Equal(t, "OpenVSwitchConfigurationUnpinned", applicationError.Type())
				require.Empty(t, fakeAgent.StartOpenVSwitchCalls)
			} else {
				require.NoError(t, err)
				require.Len(t, fakeAgent.StartOpenVSwitchCalls, 1)
			}
			require.Empty(t, fakeAgent.StartConfiguredOpenVSwitchCalls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestConfiguredOpenVSwitchRejectsUnsupportedAgentWithoutFallback(t *testing.T) {
	node := db.Node{ID: uuid.New(), NodeSetID: uuid.New(), NodePoolID: uuid.New(), Name: "node-1"}
	database := &testutil.MockDB{}
	mockOpenVSwitchNodeSet(database, node, &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"})
	fakeAgent := &agenttest.FakeClient{
		StartConfiguredOpenVSwitchHook: func(_ db.Node, _ *nvkeproto.StartConfiguredOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
			return nil, status.Error(codes.Unimplemented, "unknown method")
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	_, _, err := a.configureOpenVSwitchOnNode(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"}, node)
	var applicationError *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationError)
	require.True(t, applicationError.NonRetryable())
	require.Equal(t, "OpenVSwitchConfigurationUnsupported", applicationError.Type())
	require.Empty(t, fakeAgent.StartOpenVSwitchCalls)
	require.Empty(t, fakeAgent.GetOpenVSwitchStatusCalls)
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func TestConfiguredOpenVSwitchAcceptsAgentSelectedBackend(t *testing.T) {
	for _, backend := range []nvkeproto.OpenVSwitchReconciliationResult_NetworkBackend{
		nvkeproto.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN,
		nvkeproto.OpenVSwitchReconciliationResult_NETWORK_BACKEND_IFUPDOWN,
	} {
		t.Run(backend.String(), func(t *testing.T) {
			desired := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
			node := db.Node{ID: uuid.New(), NodeSetID: uuid.New(), NodePoolID: uuid.New()}
			database := &testutil.MockDB{}
			mockOpenVSwitchNodeSet(database, node, desired)
			fakeAgent := configuredOpenVSwitchFake(t, desired)
			valid := fakeAgent.StartConfiguredOpenVSwitchHook
			fakeAgent.StartConfiguredOpenVSwitchHook = func(node db.Node, req *nvkeproto.StartConfiguredOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
				response, err := valid(node, req)
				response.Operation.Result.NetworkBackend = backend
				return response, err
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			result, _, err := a.configureOpenVSwitchOnNode(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"}, node)
			require.NoError(t, err)
			assert.Equal(t, desired.BridgeName, result.BridgeName)
			assert.Equal(t, desired.UplinkInterface, result.UplinkInterface)
			assert.Equal(t, backend.String(), result.NetworkBackend)
			database.AssertExpectations(t)
		})
	}
}

func TestConfiguredOpenVSwitchRejectsWrongObservedNetwork(t *testing.T) {
	for _, field := range []string{"bridge", "uplink"} {
		t.Run(field, func(t *testing.T) {
			desired := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
			node := db.Node{ID: uuid.New(), NodeSetID: uuid.New(), NodePoolID: uuid.New()}
			database := &testutil.MockDB{}
			mockOpenVSwitchNodeSet(database, node, desired)
			fakeAgent := configuredOpenVSwitchFake(t, desired)
			valid := fakeAgent.StartConfiguredOpenVSwitchHook
			fakeAgent.StartConfiguredOpenVSwitchHook = func(node db.Node, req *nvkeproto.StartConfiguredOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
				response, err := valid(node, req)
				switch field {
				case "bridge":
					response.Operation.Result.BridgeName = "brmgmt0"
				case "uplink":
					response.Operation.Result.UplinkInterface = "mgmt0"
				}
				return response, err
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			_, _, err := a.configureOpenVSwitchOnNode(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"}, node)
			var applicationError *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationError)
			require.True(t, applicationError.NonRetryable())
			require.Equal(t, "OpenVSwitchConfigurationMismatch", applicationError.Type())
			database.AssertExpectations(t)
		})
	}
}

func TestConfigureOpenVSwitchKeepsOperationIDAfterAgentFailure(t *testing.T) {
	for name, desired := range map[string]*db.OpenVSwitchConfig{
		"automatic selection": nil,
		"explicit selection":  {UplinkInterface: "eth1", BridgeName: "br-eth1"},
	} {
		t.Run(name, func(t *testing.T) {
			node := db.Node{ID: uuid.New(), NodeSetID: uuid.New(), NodePoolID: uuid.New()}
			database := &testutil.MockDB{}
			database.On("GetNodeSet", mock.Anything, node.NodeSetID).Return(db.NodeSet{
				ID: node.NodeSetID, NodePoolID: node.NodePoolID, OpenVSwitch: desired,
			}, nil).Twice()
			database.On("GetNodePool", mock.Anything, node.NodePoolID).Return(db.NodePool{ID: node.NodePoolID}, nil).Twice()
			var operationIDs []string
			fakeAgent := &agenttest.FakeClient{
				StartOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
					operationIDs = append(operationIDs, req.GetOperationId())
					return failedOpenVSwitchStart(req.GetOperationId()), nil
				},
				StartConfiguredOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartConfiguredOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
					operationIDs = append(operationIDs, req.GetOperationId())
					return failedOpenVSwitchStart(req.GetOperationId()), nil
				},
			}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}
			for range 2 {
				_, _, err := a.configureOpenVSwitchOnNode(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"}, node)
				require.ErrorContains(t, err, "netplan apply failed")
			}
			require.Equal(t, []string{"op-1", "op-1"}, operationIDs)
			require.Empty(t, fakeAgent.GetOpenVSwitchStatusCalls)
			require.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestOpenVSwitchPollingFailureReturnsAgentError(t *testing.T) {
	node := db.Node{ID: uuid.New(), NodeSetID: uuid.New(), NodePoolID: uuid.New()}
	database := &testutil.MockDB{}
	mockOpenVSwitchNodeSet(database, node, nil)
	fakeAgent := &agenttest.FakeClient{
		StartOpenVSwitchHook: func(_ db.Node, req *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
			return &nvkeproto.StartOpenVSwitchReconciliationResponse{Operation: runningOpenVSwitchOperation(req.GetOperationId())}, nil
		},
		GetOpenVSwitchStatusHook: func(_ db.Node, req *nvkeproto.GetOpenVSwitchReconciliationStatusRequest) (*nvkeproto.GetOpenVSwitchReconciliationStatusResponse, error) {
			return &nvkeproto.GetOpenVSwitchReconciliationStatusResponse{Operation: failedOpenVSwitchStart(req.GetOperationId()).GetOperation()}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	_, _, err := a.configureOpenVSwitchOnNode(context.Background(), ConfigureOpenVSwitchInput{NodeID: node.ID, OperationID: "op-1"}, node)
	require.ErrorContains(t, err, "netplan apply failed")
	require.Len(t, fakeAgent.StartOpenVSwitchCalls, 1, "a reported failure must not start another agent operation")
	require.Empty(t, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}

func failedOpenVSwitchStart(operationID string) *nvkeproto.StartOpenVSwitchReconciliationResponse {
	return &nvkeproto.StartOpenVSwitchReconciliationResponse{Operation: &nvkeproto.OpenVSwitchReconciliationOperation{
		Operation: &nvkeproto.NodeAgentOperationStatus{
			OperationId:  operationID,
			Phase:        nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			ErrorMessage: "netplan apply failed",
		},
	}}
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
