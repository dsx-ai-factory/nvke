// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
)

// A PATCH that omits a field leaves it unchanged. These go through the real
// NewServer so the OpenAPI request validator runs: a schema default on an
// UpdateNodePoolRequest property is written into the body before the handler
// sees it, which turned an omitted maxSurge into an explicit 0.
func TestUpdateNodePoolMaxSurge(t *testing.T) {
	t.Parallel()

	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")

	tests := []struct {
		name              string
		body              string
		wantMaxSurge      int
		wantDesiredCount  int
		wantRecycleBudget int
	}{
		{
			name:              "omitted maxSurge is unchanged when another field is sent",
			body:              `{"recycleBudget":1}`,
			wantMaxSurge:      1,
			wantDesiredCount:  1,
			wantRecycleBudget: 1,
		},
		{
			name:             "omitted maxSurge is unchanged when only desiredNodeCount is sent",
			body:             `{"desiredNodeCount":2}`,
			wantMaxSurge:     1,
			wantDesiredCount: 2,
		},
		{
			name:             "explicit maxSurge 0 is applied",
			body:             `{"maxSurge":0}`,
			wantMaxSurge:     0,
			wantDesiredCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored := db.NodePool{
				ID:           nodePoolID,
				ClusterID:    clusterID,
				DesiredCount: 1,
				MaxSurge:     1,
			}

			mockDB := &mockDB{MockTx: &mockTx{}}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			mockDB.MockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(stored, nil)
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(forgeDBCluster(db.Cluster{
				ID:          clusterID,
				PodCIDR:     "10.240.0.0/16",
				ServiceCIDR: "10.120.0.0/24",
				Nico:        &db.NicoClusterProvider{SiteID: uuid.New()},
			}), nil)
			mockDB.MockTx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(stored, nil)
			mockDB.MockTx.On("ListNodePools", mock.Anything, mock.Anything).Maybe().Return([]db.NodePool{stored}, nil)

			// Return what the handler saved, so the 200 body reflects the update.
			var saved *db.NodePool
			call := mockDB.MockTx.On("UpdateNodePool", mock.Anything, mock.AnythingOfType("*db.NodePool"))
			call.Run(func(args mock.Arguments) {
				n := *args.Get(1).(*db.NodePool)
				saved = &n
				call.ReturnArguments = mock.Arguments{n, nil}
			}).Return(db.NodePool{}, nil)

			srv, err := server.NewServer(config.Config{ProfilesDir: "./profiles"},
				slog.New(slog.DiscardHandler), mockDB, nil, nil, nil)
			require.NoError(t, err)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPatch,
				"/api/nodePools/"+nodePoolID.String(), strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			require.NotNil(t, saved, "the handler must store the updated node pool")
			require.Equal(t, tt.wantMaxSurge, saved.MaxSurge, "maxSurge")
			require.Equal(t, tt.wantDesiredCount, saved.DesiredCount, "desiredNodeCount")
			require.Equal(t, tt.wantRecycleBudget, saved.RecycleBudget, "recycleBudget")

			var got api.NodePool
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
			gotMaxSurge := 0 // the API omits maxSurge when it is 0
			if got.MaxSurge != nil {
				gotMaxSurge = *got.MaxSurge
			}
			require.Equal(t, tt.wantMaxSurge, gotMaxSurge, "response maxSurge")
			require.Equal(t, tt.wantDesiredCount, got.DesiredNodeCount, "response desiredNodeCount")
			require.Equal(t, tt.wantRecycleBudget, got.RecycleBudget, "response recycleBudget")
		})
	}
}
