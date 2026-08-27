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

func TestReconfigureKubeletAPIServer(t *testing.T) {
	tests := []struct {
		name     string
		response *nkeproto.ReconfigureKubeletAPIServerResponse
		rpcErr   error
		wantErr  string
	}{
		{
			name: "applied",
			response: &nkeproto.ReconfigureKubeletAPIServerResponse{
				Outcome: nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_APPLIED,
			},
		},
		{
			name: "unchanged",
			response: &nkeproto.ReconfigureKubeletAPIServerResponse{
				Outcome: nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_UNCHANGED,
			},
		},
		{
			name: "failed restored",
			response: &nkeproto.ReconfigureKubeletAPIServerResponse{
				Outcome:      nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_RESTORED,
				ErrorMessage: "restart failed",
			},
			wantErr: "previous state was restored: restart failed",
		},
		{
			name: "failed not restored",
			response: &nkeproto.ReconfigureKubeletAPIServerResponse{
				Outcome:      nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED,
				ErrorMessage: "rollback failed",
			},
			wantErr: "previous state was not restored: rollback failed",
		},
		{
			name:     "RPC failure",
			rpcErr:   errors.New("dial failed"),
			wantErr:  "dial failed",
			response: &nkeproto.ReconfigureKubeletAPIServerResponse{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nodeID := uuid.New()
			node := db.Node{ID: nodeID, Name: "node-1"}
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				ReconfigureKubeletAPIServerResponse: test.response,
				ReconfigureKubeletAPIServerErr:      test.rpcErr,
			}
			activity := &Activities{db: database, nodeAgentClient: fakeAgent}

			_, err := activity.ReconfigureKubeletAPIServer(context.Background(), ReconfigureKubeletAPIServerInput{
				NodeID: nodeID,
				Server: "https://api.example.com:6443",
			})

			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []uuid.UUID{nodeID}, fakeAgent.ReconfigureKubeletAPIServerCalls)
			require.Len(t, fakeAgent.ReconfigureKubeletAPIServerRequests, 1)
			assert.Equal(t, "https://api.example.com:6443", fakeAgent.ReconfigureKubeletAPIServerRequests[0].GetServer())
			assert.Empty(t, fakeAgent.ExecuteScriptNames)
			database.AssertExpectations(t)
		})
	}
}

func TestReconfigureKubeletAPIServerFallsBackToLegacyRunbooksWhenRPCIsUnimplemented(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "node-1"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Times(3)
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-1"}, nil).Twice()
	fakeAgent := &agenttest.FakeClient{
		ReconfigureKubeletAPIServerErr: status.Error(codes.Unimplemented, "unknown method ReconfigureKubeletAPIServer"),
		ExecuteScriptHook: func(_ db.Node, scriptName, scriptContents string) (*nkeproto.ExecuteScriptResponse, error) {
			switch scriptName {
			case legacyKubeletAPIAddressRunbook:
				require.Contains(t, scriptContents, "export API_SERVER='https://api.example.com:6443'")
				require.Contains(t, scriptContents, "update_kubelet_api_address")
			case legacyRestartKubeletRunbook:
				require.Contains(t, scriptContents, "systemctl restart kubelet")
			default:
				t.Fatalf("unexpected legacy runbook %q", scriptName)
			}
			return &nkeproto.ExecuteScriptResponse{}, nil
		},
	}
	a := &Activities{db: database, nodeAgentClient: fakeAgent}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ReconfigureKubeletAPIServer)

	value, err := env.ExecuteActivity(a.ReconfigureKubeletAPIServer, ReconfigureKubeletAPIServerInput{
		NodeID: nodeID,
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	var result ReconfigureKubeletAPIServerResult
	require.NoError(t, value.Get(&result))
	require.Equal(t, []uuid.UUID{nodeID}, fakeAgent.ReconfigureKubeletAPIServerCalls)
	require.Equal(t, []string{legacyKubeletAPIAddressRunbook, legacyRestartKubeletRunbook}, fakeAgent.ExecuteScriptNames)
	database.AssertExpectations(t)
}
