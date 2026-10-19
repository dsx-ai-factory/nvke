// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func provisionedSpireCluster(id uuid.UUID) db.Cluster {
	c := db.Cluster{ID: id, Name: "tenant-1"}
	c.Conditions.Provisioned = db.NewCondition(true, "")
	return c
}

func TestUpdateClusterSpireServer(t *testing.T) {
	clusterID := uuid.New()
	target := api.SpireServer{Address: "spire.example.test", Port: 8081}
	startedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		body         *api.SpireServer
		cluster      db.Cluster
		getErr       error
		wantEnqueue  bool
		wantResponse any
		wantMessage  string
	}{
		{name: "missing body", body: nil, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "request body is required"},
		{name: "empty address", body: &api.SpireServer{Port: 8081}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "address is required"},
		{name: "address with whitespace", body: &api.SpireServer{Address: "spire server", Port: 8081}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "not a host name"},
		{name: "address with quote", body: &api.SpireServer{Address: `spire"`, Port: 8081}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "not a host name"},
		{name: "address with control character", body: &api.SpireServer{Address: "spire\n", Port: 8081}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "not a host name"},
		{name: "address with uppercase letters", body: &api.SpireServer{Address: "Spire.Example.com", Port: 8081}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "not a host name"},
		{name: "address with an empty label", body: &api.SpireServer{Address: "spire..example.com", Port: 8081}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "not a host name"},
		{name: "port zero", body: &api.SpireServer{Address: "10.0.0.1", Port: 0}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "out of range"},
		{name: "port too large", body: &api.SpireServer{Address: "fd00::1", Port: 65536}, wantResponse: api.UpdateClusterSpireServer400JSONResponse{}, wantMessage: "out of range"},
		{name: "cluster not found", body: &target, getErr: db.ErrNotFound, wantResponse: api.UpdateClusterSpireServer404Response{}},
		{
			name: "another operation in progress",
			body: &target,
			cluster: func() db.Cluster {
				c := provisionedSpireCluster(clusterID)
				db.SetUpdateInProgress(&c, db.OperationTypeOIDCEnable, "", startedAt)
				return c
			}(),
			wantResponse: api.UpdateClusterSpireServer409JSONResponse{},
			wantMessage:  "oidc_enable in progress",
		},
		{
			name: "same update in progress is accepted without enqueuing",
			body: &target,
			cluster: func() db.Cluster {
				c := provisionedSpireCluster(clusterID)
				c.SpireServer = &db.ClusterSpireServer{Address: target.Address, Port: target.Port}
				db.SetUpdateInProgress(&c, db.OperationTypeSpireServerUpdate, "", startedAt)
				return c
			}(),
			wantResponse: api.UpdateClusterSpireServer202Response{},
		},
		{
			name: "update to a different server in progress",
			body: &target,
			cluster: func() db.Cluster {
				c := provisionedSpireCluster(clusterID)
				c.SpireServer = &db.ClusterSpireServer{Address: "old.example.test", Port: target.Port}
				db.SetUpdateInProgress(&c, db.OperationTypeSpireServerUpdate, "", startedAt)
				return c
			}(),
			wantResponse: api.UpdateClusterSpireServer409JSONResponse{},
			wantMessage:  "different server",
		},
		{
			name: "cluster deleting",
			body: &target,
			cluster: func() db.Cluster {
				c := provisionedSpireCluster(clusterID)
				c.Conditions.Deleting = db.NewCondition(true, "")
				return c
			}(),
			wantResponse: api.UpdateClusterSpireServer409JSONResponse{},
			wantMessage:  "deletion",
		},
		{
			name:         "cluster not provisioned",
			body:         &target,
			cluster:      db.Cluster{ID: clusterID},
			wantResponse: api.UpdateClusterSpireServer409JSONResponse{},
			wantMessage:  "provisioned",
		},
		{
			name: "previous failure is cleared and the workflow enqueued",
			body: &target,
			cluster: func() db.Cluster {
				c := provisionedSpireCluster(clusterID)
				db.SetUpdateFailed(&c, "earlier failure")
				return c
			}(),
			wantEnqueue:  true,
			wantResponse: api.UpdateClusterSpireServer202Response{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockTx := &testutil.MockTx{}
			mockDB := &testutil.MockDB{MockTx: mockTx}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
			mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(tt.cluster, tt.getErr).Maybe()
			if tt.wantEnqueue {
				mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, constants.UpdateSpireServerWorkflowType,
					constants.UpdateSpireServerWorkflowPrefix+clusterID.String(),
					map[string]any{clusterIDConst: clusterID, "address": target.Address, "port": target.Port},
				).Return(db.WorkflowOutbox{}, nil).Once()
				mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
					return c.SpireServer != nil && c.SpireServer.Address == target.Address && c.SpireServer.Port == target.Port &&
						c.Conditions.UpdateInProgress != nil && c.Conditions.UpdateInProgress.Type == db.OperationTypeSpireServerUpdate &&
						c.Conditions.UpdateFailed == nil
				})).Return(db.Cluster{}, nil).Once()
			}

			s := &clusterService{logger: slog.New(slog.DiscardHandler), db: mockDB}
			resp, err := s.UpdateClusterSpireServer(context.Background(), api.UpdateClusterSpireServerRequestObject{ClusterID: clusterID, Body: tt.body})
			require.NoError(t, err)
			assert.IsType(t, tt.wantResponse, resp)
			switch r := resp.(type) {
			case api.UpdateClusterSpireServer400JSONResponse:
				assert.Contains(t, r.Message, tt.wantMessage)
			case api.UpdateClusterSpireServer409JSONResponse:
				assert.Contains(t, r.Message, tt.wantMessage)
			}
			mockTx.AssertExpectations(t)
			if !tt.wantEnqueue {
				mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
			}
		})
	}
}

func TestGetClusterSpireServer(t *testing.T) {
	clusterID := uuid.New()
	okNode := db.Node{ID: uuid.New(), Name: "ok", IP: "10.0.0.1", SpiffeEnabled: true}
	errNode := db.Node{ID: uuid.New(), Name: "err", IP: "10.0.0.2", SpiffeEnabled: true}
	slowNode := db.Node{ID: uuid.New(), Name: "slow", IP: "10.0.0.3", SpiffeEnabled: true}
	noIPNode := db.Node{ID: uuid.New(), Name: "no-ip"}
	expires := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

	fake := &agenttest.FakeClient{GetSpireAgentStatusHook: func(ctx context.Context, node db.Node) (*nvkeproto.GetSpireAgentStatusResponse, error) {
		switch node.Name {
		case "ok":
			return &nvkeproto.GetSpireAgentStatusResponse{
				ServerAddress: "spire.example.test", ServerPort: 8081, ConfigApplied: true,
				HasValidSvid: true, SvidExpiresAtUnixSeconds: expires.Unix(),
			}, nil
		case "slow":
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			return nil, errors.New("connection refused")
		}
	}}

	tests := []struct {
		name      string
		client    *agenttest.FakeClient
		cluster   db.Cluster
		getErr    error
		nodes     []db.Node
		want404   bool
		assertion func(t *testing.T, got api.ClusterSpireServerStatus)
	}{
		{name: "cluster not found", getErr: db.ErrNotFound, want404: true},
		{
			name:    "reachable, error, timeout and no IP",
			client:  fake,
			cluster: db.Cluster{ID: clusterID, SpireServer: &db.ClusterSpireServer{Address: "spire.example.test", Port: 8081}},
			nodes:   []db.Node{okNode, errNode, slowNode, noIPNode},
			assertion: func(t *testing.T, got api.ClusterSpireServerStatus) {
				t.Helper()
				require.Equal(t, &api.SpireServer{Address: "spire.example.test", Port: 8081}, got.Requested)
				require.Len(t, got.Nodes, 4)
				ok := got.Nodes[0]
				assert.True(t, ok.Reachable)
				assert.True(t, ok.HasValidSVID)
				assert.Equal(t, "spire.example.test", *ok.ServerAddress)
				assert.Equal(t, 8081, *ok.ServerPort)
				assert.Equal(t, expires, *ok.SvidExpiresAt)
				assert.Nil(t, ok.Message)
				for _, n := range got.Nodes[1:] {
					assert.False(t, n.Reachable, n.Name)
					assert.NotNil(t, n.Message, n.Name)
				}
				assert.Contains(t, *got.Nodes[1].Message, "connection refused")
				assert.Contains(t, *got.Nodes[2].Message, "deadline exceeded")
			},
		},
		{
			name:    "nil client reports nodes unreachable",
			cluster: db.Cluster{ID: clusterID},
			nodes:   []db.Node{okNode},
			assertion: func(t *testing.T, got api.ClusterSpireServerStatus) {
				t.Helper()
				assert.Nil(t, got.Requested)
				require.Len(t, got.Nodes, 1)
				assert.False(t, got.Nodes[0].Reachable)
				assert.Equal(t, okNode.ID, got.Nodes[0].NodeID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(tt.cluster, tt.getErr)
			mockDB.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID}).Return(tt.nodes, nil).Maybe()

			s := &clusterService{logger: slog.New(slog.DiscardHandler), db: mockDB}
			if tt.client != nil {
				s.nodeAgentClient = tt.client
			}
			// The request deadline is shorter than spireStatusTimeout, so the slow
			// node times out quickly.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			resp, err := s.GetClusterSpireServer(ctx, api.GetClusterSpireServerRequestObject{ClusterID: clusterID})
			require.NoError(t, err)
			if tt.want404 {
				assert.IsType(t, api.GetClusterSpireServer404Response{}, resp)
				return
			}
			require.IsType(t, api.GetClusterSpireServer200JSONResponse{}, resp)
			tt.assertion(t, api.ClusterSpireServerStatus(resp.(api.GetClusterSpireServer200JSONResponse)))
		})
	}
}

func TestValidateSpireServer(t *testing.T) {
	for _, address := range []string{"spire", "spire-server.example.test", "192.168.150.100", "fd00::1"} {
		require.NoError(t, validateSpireServer(api.SpireServer{Address: address, Port: 1}), address)
	}
	for _, address := range []string{"-spire", "spire.", "spire:8081", "spire;rm", "spire\t", "http://spire"} {
		assert.Error(t, validateSpireServer(api.SpireServer{Address: address, Port: 1}), address)
	}
}
