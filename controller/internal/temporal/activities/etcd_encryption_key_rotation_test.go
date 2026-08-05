// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
	"github.com/NVIDIA/nke/controller/internal/testutil"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

func TestBeginEtcdEncryptionKeyRotation_AcquiresClusterMutationLock(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		EtcdEncrypted: true,
		Conditions: db.ClusterConditions{
			Provisioned:  db.NewCondition(true, "ready"),
			UpdateFailed: conditionPtr(db.NewCondition(true, "prior failure")),
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
		return cluster.ID == clusterID &&
			cluster.EtcdEncrypted &&
			cluster.Conditions.UpdateFailed == nil &&
			cluster.Conditions.UpdateInProgress != nil &&
			cluster.Conditions.UpdateInProgress.Type == db.OperationTypeEtcdEncryption &&
			cluster.Conditions.UpdateInProgress.Message == "rotating etcd encryption key"
	})).Return(db.Cluster{}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)

	_, err := a.BeginEtcdEncryptionKeyRotation(
		context.Background(),
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	)

	require.NoError(t, err)
	mockDB.MockTx.AssertExpectations(t)
}

func TestBeginEtcdEncryptionKeyRotation_RejectsExistingClusterMutation(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		EtcdEncrypted: true,
		Conditions: db.ClusterConditions{
			Provisioned: db.NewCondition(true, "ready"),
			UpdateInProgress: &db.UpdateInProgress{
				Type: db.OperationTypeK8sUpgrade,
			},
		},
	}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)

	_, err := a.BeginEtcdEncryptionKeyRotation(
		context.Background(),
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	)

	require.ErrorContains(t, err, "k8s_upgrade is in progress")
	mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestPrepareEtcdEncryptionKeyRotation_RequiresEncryptedCluster(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil).Once()
	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)

	_, err := a.PrepareEtcdEncryptionKeyRotation(
		context.Background(),
		activities.PrepareEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	)

	require.ErrorContains(t, err, "etcd encryption is not enabled")
	mockDB.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
}

func TestCompleteEtcdEncryptionKeyRotation_PreservesEncryptionAndClearsProgress(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:            clusterID,
		EtcdEncrypted: true,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption},
			UpdateFailed:     conditionPtr(db.NewCondition(true, "previous failure")),
		},
	}, nil).Once()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
		return cluster.ID == clusterID &&
			cluster.EtcdEncrypted &&
			cluster.Conditions.UpdateInProgress == nil &&
			cluster.Conditions.UpdateFailed == nil
	})).Return(db.Cluster{}, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	a := activities.NewActivities(config.Config{}, mockDB, nil, nil, nil, "", nil)
	_, err := a.CompleteEtcdEncryptionKeyRotation(
		context.Background(),
		activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	)

	require.NoError(t, err)
	mockDB.MockTx.AssertExpectations(t)
}

func TestStartEtcdEncryptionRewrite_StartsFreshOperationAfterPriorSuccess(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "cp-0"}
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	nodeAgent := &agenttest.FakeClient{
		GetNodeInfoResp: &nkeproto.NodeInfoResponse{
			JsonResponse: `{"k8sAPIRewriteOperation":{"id":"enablement-op","state":"succeeded"}}`,
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
		NodeID: node.ID, OperationID: "rotation-op",
	})

	require.NoError(t, err)
	require.Equal(t, "rotation-op", operation.OperationID)
	require.Equal(t, activities.EtcdEncryptionOperationPhaseRunning, operation.Phase)
	require.Equal(t, []string{"rotation-op"}, nodeAgent.StartEncryptedObjectsRewriteIDs)
	mockDB.AssertExpectations(t)
}
