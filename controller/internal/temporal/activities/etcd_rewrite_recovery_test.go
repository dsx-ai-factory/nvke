// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestGetEtcdEncryptionRewriteStatus_RecoveryAndValidation(t *testing.T) {
	const operationID = "acknowledged-operation"
	running := pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING
	succeeded := pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED
	failed := pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_FAILED
	op := func(id string, state pb.EtcdEncryptionAtRestObjectsRewriteState) *pb.EtcdEncryptionAtRestObjectsRewriteOperation {
		return &pb.EtcdEncryptionAtRestObjectsRewriteOperation{OperationId: id, State: state, ErrorMessage: "upstream-body-marker"}
	}

	tests := []struct {
		name      string
		status    *pb.EtcdEncryptionAtRestObjectsRewriteOperation
		statusErr error
		start     *pb.EtcdEncryptionAtRestObjectsRewriteOperation
		startErr  error
		wantStart bool
		wantPhase string
		wantError string
	}{
		{name: "running recovers same operation", status: op(operationID, running), start: op(operationID, running), wantStart: true, wantPhase: activities.EtcdEncryptionOperationPhaseRunning},
		{name: "completed during recovery", status: op(operationID, running), start: op(operationID, succeeded), wantStart: true, wantPhase: activities.EtcdEncryptionOperationPhaseSucceeded},
		{name: "failed during recovery", status: op(operationID, running), start: op(operationID, failed), wantStart: true, wantPhase: activities.EtcdEncryptionOperationPhaseFailed},
		{name: "success remains untouched", status: op(operationID, succeeded), wantPhase: activities.EtcdEncryptionOperationPhaseSucceeded},
		{name: "failure remains untouched", status: op(operationID, failed), wantPhase: activities.EtcdEncryptionOperationPhaseFailed},
		{name: "missing operation", statusErr: status.Error(codes.NotFound, "upstream-body-marker"), wantError: "request failed"},
		{name: "status transport failure", statusErr: errors.New("upstream-body-marker"), wantError: "request failed"},
		{name: "nil status", wantError: "unexpected operation"},
		{name: "foreign running status", status: op("foreign", running), wantError: "unexpected operation"},
		{name: "foreign success status", status: op("foreign", succeeded), wantError: "unexpected operation"},
		{name: "unspecified status", status: op(operationID, 0), wantError: "unknown phase"},
		{name: "unknown status", status: op(operationID, 99), wantError: "unknown phase"},
		{name: "recovery transport failure", status: op(operationID, running), startErr: errors.New("upstream-body-marker"), wantStart: true, wantError: "request failed"},
		{name: "nil recovery result", status: op(operationID, running), wantStart: true, wantError: "unexpected operation"},
		{name: "foreign recovery result", status: op(operationID, running), start: op("foreign", succeeded), wantStart: true, wantError: "unexpected operation"},
		{name: "unknown recovery result", status: op(operationID, running), start: op(operationID, 99), wantStart: true, wantError: "unknown phase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := db.Node{ID: uuid.New(), Name: "cp-0"}
			store := &testutil.MockDB{}
			store.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			nodeAgent := &agenttest.FakeClient{
				GetEncryptedObjectsRewriteHook: func(gotNode db.Node, id string) (*pb.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
					require.Equal(t, node.ID, gotNode.ID)
					require.Equal(t, operationID, id)
					return tt.status, tt.statusErr
				},
				StartEncryptedObjectsRewriteHook: func(gotNode db.Node, id string) (*pb.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
					require.Equal(t, node.ID, gotNode.ID)
					require.Equal(t, operationID, id)
					return tt.start, tt.startErr
				},
			}
			a := activities.NewActivities(config.Config{}, store, nil, nodeAgent, "", nil, "")
			result, err := a.GetEtcdEncryptionRewriteStatus(t.Context(), activities.EtcdEncryptionRewriteInput{NodeID: node.ID, OperationID: operationID})
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				require.NotContains(t, err.Error(), "upstream-body-marker")
				require.Empty(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, operationID, result.OperationID)
				require.Equal(t, tt.wantPhase, result.Phase)
				require.Empty(t, result.ErrorMessage)
			}
			if tt.wantStart {
				require.Equal(t, []string{operationID}, nodeAgent.StartEncryptedObjectsRewriteIDs)
			} else {
				require.Empty(t, nodeAgent.StartEncryptedObjectsRewriteIDs)
			}
			store.AssertExpectations(t)
		})
	}
}

func TestStartEtcdEncryptionRewrite_DoesNotAdoptForeignResult(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "cp-0"}
	store := &testutil.MockDB{}
	store.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	nodeAgent := &agenttest.FakeClient{
		GetEncryptedObjectsRewriteHook: func(db.Node, string) (*pb.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			return nil, status.Error(codes.NotFound, "not found")
		},
		StartEncryptedObjectsRewriteHook: func(db.Node, string) (*pb.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			return &pb.EtcdEncryptionAtRestObjectsRewriteOperation{
				OperationId: "foreign",
				State:       pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED,
			}, nil
		},
	}
	a := activities.NewActivities(config.Config{}, store, nil, nodeAgent, "", nil, "")
	result, err := a.StartEtcdEncryptionRewrite(t.Context(), activities.EtcdEncryptionRewriteInput{NodeID: node.ID, OperationID: "requested"})
	require.ErrorContains(t, err, "unexpected operation")
	require.Empty(t, result)
	store.AssertExpectations(t)
}
