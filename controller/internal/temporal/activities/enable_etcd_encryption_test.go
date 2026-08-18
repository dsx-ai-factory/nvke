// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestPrepareEtcdEncryption_ReturnsStableNodeOrderAndHealthyExecutor(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodeIDs := []uuid.UUID{
		uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
		uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"),
	}
	nodes := []db.Node{
		{
			ID: nodeIDs[2], ClusterID: clusterID, Name: "control-plane-3", IP: "192.0.2.3", ControlPlane: true,
			Conditions: db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}},
		},
		{
			ID: nodeIDs[0], ClusterID: clusterID, Name: "control-plane-1", IP: "192.0.2.1", ControlPlane: true,
			Conditions: db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}},
		},
		{
			ID: nodeIDs[1], ClusterID: clusterID, Name: "control-plane-2", IP: "192.0.2.2", ControlPlane: true,
			Conditions: db.NodeConditions{
				Joined:          db.Condition{Status: db.ConditionStatusTrue},
				KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			},
		},
	}

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return(nodes, nil).Once()
	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)

	result, err := a.PrepareEtcdEncryption(context.Background(), activities.PrepareEtcdEncryptionInput{
		ClusterID: clusterID,
	})
	require.NoError(t, err)
	require.Equal(t, nodeIDs, result.NodeIDs)
	require.Equal(t, nodeIDs[1], result.ExecutorNodeID)
	mockDB.AssertExpectations(t)
}

func TestValidateEtcdEncryptionHealth_RejectsOldNodeAgent(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	node := db.Node{
		ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
		ClusterID:    clusterID,
		Name:         "control-plane-1",
		IP:           "192.0.2.1",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Joined:          db.Condition{Status: db.ConditionStatusTrue},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{node}, nil).Once()
	nodeAgent := &agenttest.FakeClient{AgentVersion: "0.1.25"}
	a := activities.NewActivities(config.Config{}, mockDB, nil, nodeAgent, nil, "", nil)

	err := a.ValidateEtcdEncryptionHealth(context.Background(), activities.ValidateEtcdEncryptionHealthInput{
		ClusterID:       clusterID,
		ExpectedNodeIDs: []uuid.UUID{node.ID},
	})
	require.ErrorContains(t, err, "older than required version 0.1.26")
	mockDB.AssertExpectations(t)
}

func TestCompleteEtcdEncryption_MarksEncryptedAndClearsOwnProgress(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption},
			UpdateFailed:     conditionPtr(db.NewCondition(true, "previous failure")),
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID &&
			c.EtcdEncrypted &&
			c.Conditions.UpdateInProgress == nil &&
			c.Conditions.UpdateFailed == nil
	})).Return(db.Cluster{}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)
	_, err := a.CompleteEtcdEncryption(context.Background(), activities.CompleteEtcdEncryptionInput{ClusterID: clusterID})
	require.NoError(t, err)
	mockDB.MockTx.AssertExpectations(t)
}

func TestClearEtcdEncryptionUpdateInProgress_ClearsOnlyEtcdEncryption(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption},
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID &&
			!c.EtcdEncrypted &&
			c.Conditions.UpdateInProgress == nil &&
			c.Conditions.UpdateFailed == nil
	})).Return(db.Cluster{}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)
	_, err := a.ClearEtcdEncryptionUpdateInProgress(
		context.Background(),
		activities.ClearEtcdEncryptionUpdateInProgressInput{ClusterID: clusterID},
	)
	require.NoError(t, err)
	mockDB.MockTx.AssertExpectations(t)
}

func TestClearEtcdEncryptionUpdateInProgress_RecordsFailure(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption},
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID &&
			!c.EtcdEncrypted &&
			c.Conditions.UpdateInProgress == nil &&
			c.Conditions.UpdateFailed != nil &&
			c.Conditions.UpdateFailed.Status == db.ConditionStatusTrue &&
			c.Conditions.UpdateFailed.Message == "etcd encryption failed"
	})).Return(db.Cluster{}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)
	_, err := a.ClearEtcdEncryptionUpdateInProgress(
		context.Background(),
		activities.ClearEtcdEncryptionUpdateInProgressInput{
			ClusterID:      clusterID,
			FailureMessage: "etcd encryption failed",
		},
	)
	require.NoError(t, err)
	mockDB.MockTx.AssertExpectations(t)
}

func TestClearEtcdEncryptionUpdateInProgress_DoesNotClearOtherOperations(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeK8sUpgrade},
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)
	_, err := a.ClearEtcdEncryptionUpdateInProgress(
		context.Background(),
		activities.ClearEtcdEncryptionUpdateInProgressInput{ClusterID: clusterID},
	)
	require.NoError(t, err)
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestStartEtcdEncryptionRewrite_ResumesRunningOperation(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "cp-0"}
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	nodeAgent := &agenttest.FakeClient{
		GetEncryptedObjectsRewriteHook: func(_ db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			return &nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{
				OperationId: operationID,
				State:       nkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING,
			}, nil
		},
		StartEncryptedObjectsRewriteHook: func(_ db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			return &nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{
				OperationId: operationID,
				State:       nkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING,
			}, nil
		},
	}
	a := activities.NewActivities(config.Config{}, mockDB, nil, nodeAgent, nil, "", nil)

	operation, err := a.StartEtcdEncryptionRewrite(context.Background(), activities.EtcdEncryptionRewriteInput{
		NodeID: node.ID, OperationID: "existing-op",
	})

	require.NoError(t, err)
	require.Equal(t, "existing-op", operation.OperationID)
	require.Equal(t, activities.EtcdEncryptionOperationPhaseRunning, operation.Phase)
	require.Equal(t, []string{"existing-op"}, nodeAgent.StartEncryptedObjectsRewriteIDs)
	mockDB.AssertExpectations(t)
}

func TestStartEtcdEncryptionRewrite_StartsFreshOperationAfterFailure(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "cp-0"}
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	nodeAgent := &agenttest.FakeClient{
		GetEncryptedObjectsRewriteHook: func(_ db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			return nil, status.Errorf(codes.NotFound, "rewrite operation %s not found", operationID)
		},
		StartEncryptedObjectsRewriteHook: func(_ db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			return &nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{
				OperationId: operationID,
				State:       nkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING,
			}, nil
		},
	}
	a := activities.NewActivities(config.Config{}, mockDB, nil, nodeAgent, nil, "", nil)

	operation, err := a.StartEtcdEncryptionRewrite(context.Background(), activities.EtcdEncryptionRewriteInput{
		NodeID: node.ID, OperationID: "new-op",
	})

	require.NoError(t, err)
	require.Equal(t, "new-op", operation.OperationID)
	require.Equal(t, activities.EtcdEncryptionOperationPhaseRunning, operation.Phase)
	require.Equal(t, []string{"new-op"}, nodeAgent.StartEncryptedObjectsRewriteIDs)
	mockDB.AssertExpectations(t)
}

func conditionPtr(condition db.Condition) *db.Condition {
	return &condition
}
