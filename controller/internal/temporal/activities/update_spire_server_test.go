// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestReconfigureNodeSpireAgent(t *testing.T) {
	tests := []struct {
		name     string
		response *nvkeproto.ReconfigureSpireAgentResponse
		rpcErr   error
		wantErr  string
	}{
		{name: "applied", response: &nvkeproto.ReconfigureSpireAgentResponse{Outcome: nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_APPLIED}},
		{name: "unchanged", response: &nvkeproto.ReconfigureSpireAgentResponse{Outcome: nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_UNCHANGED}},
		{
			name:     "failed",
			response: &nvkeproto.ReconfigureSpireAgentResponse{Outcome: nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_FAILED, ErrorMessage: "no SVID after 3m"},
			wantErr:  "no SVID after 3m",
		},
		{name: "unspecified", response: &nvkeproto.ReconfigureSpireAgentResponse{}, wantErr: "unspecified"},
		{name: "RPC failure", rpcErr: errors.New("dial failed"), wantErr: "dial failed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nodeID := uuid.New()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID, Name: "node-1"}, nil).Once()
			var gotReq *nvkeproto.ReconfigureSpireAgentRequest
			fakeAgent := &agenttest.FakeClient{ReconfigureSpireAgentHook: func(_ context.Context, _ db.Node, req *nvkeproto.ReconfigureSpireAgentRequest) (*nvkeproto.ReconfigureSpireAgentResponse, error) {
				gotReq = req
				return test.response, test.rpcErr
			}}
			a := &Activities{db: database, nodeAgentClient: fakeAgent}

			_, err := a.ReconfigureNodeSpireAgent(context.Background(), ReconfigureNodeSpireAgentInput{NodeID: nodeID, Address: "spire.example.test", Port: 8081})

			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, "spire.example.test", gotReq.GetServerAddress())
			assert.Equal(t, uint32(8081), gotReq.GetServerPort())
			database.AssertExpectations(t)
		})
	}
}

func TestClearSpireServerUpdateInProgress(t *testing.T) {
	startedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		op             db.OperationType
		failureMessage string
		wantWrite      bool
	}{
		{name: "success clears the lock and UpdateFailed", op: db.OperationTypeSpireServerUpdate, wantWrite: true},
		{name: "failure clears the lock and records UpdateFailed", op: db.OperationTypeSpireServerUpdate, failureMessage: "failed on nodes: a", wantWrite: true},
		{name: "another operation's lock is left alone", op: db.OperationTypeOIDCEnable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clusterID := uuid.New()
			cluster := db.Cluster{ID: clusterID}
			db.SetUpdateInProgress(&cluster, test.op, "", startedAt)
			db.SetUpdateFailed(&cluster, "earlier")

			mockTx := &testutil.MockTx{}
			mockDB := &testutil.MockDB{MockTx: mockTx}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
			mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
				if c.Conditions.UpdateInProgress != nil {
					return false
				}
				if test.failureMessage == "" {
					return c.Conditions.UpdateFailed == nil
				}
				return c.Conditions.UpdateFailed != nil && c.Conditions.UpdateFailed.Message == test.failureMessage
			})).Return(db.Cluster{}, nil).Maybe()

			a := &Activities{db: mockDB}
			_, err := a.ClearSpireServerUpdateInProgress(context.Background(), ClearSpireServerUpdateInProgressInput{ClusterID: clusterID, FailureMessage: test.failureMessage})
			require.NoError(t, err)
			if test.wantWrite {
				mockTx.AssertNumberOfCalls(t, "UpdateCluster", 1)
			} else {
				mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
			}
		})
	}
}
