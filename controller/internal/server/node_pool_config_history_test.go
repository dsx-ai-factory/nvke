// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestGetNodePoolConfigHistory(t *testing.T) {
	nodePoolID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	nodeSetID1 := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001")
	nodeSetID2 := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	nodeSetID3 := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000003")

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	baseNodeSet := db.NodeSet{
		NodePoolID:           nodePoolID,
		KubernetesVersion:    "1.34.2",
		SystemReservedMemory: "16Gi",
		KubeReservedMemory:   "2Gi",
		TeleportEnabled:      false,
		Nico: &db.NicoNodePoolProvider{
			OSProfile:             "nke-1.34.2-251201-arm64-ytl",
			ConfigProfile:         "nvcf-cp-v3",
			DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
			SubnetIDs:             []uuid.UUID{},
			VpcPrefixIDs:          []uuid.UUID{},
		},
	}

	makeNodeSet := func(id uuid.UUID, createdAt time.Time, mutate func(*db.NodeSet)) db.NodeSet {
		ns := baseNodeSet
		ns.ID = id
		ns.CreatedAt = createdAt
		// The base fixture's payload pointer is shared across every node set built
		// here, so each needs its own copy before a mutator touches one.
		nico, err := ns.NicoProvider()
		require.NoError(t, err)
		ns.Nico = &nico
		if mutate != nil {
			mutate(&ns)
		}
		return ns
	}

	tests := []struct {
		name           string
		nodePoolID     uuid.UUID
		setupMock      func(*testutil.MockDB)
		wantStatusCode int
		assertBody     func(*testing.T, *http.Response)
	}{
		{
			name:       "node pool not found returns 404",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound)
			},
			wantStatusCode: http.StatusNotFound,
		},
		{
			name:       "GetNodePool db error returns 500",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{}, fmt.Errorf("db unavailable"))
			},
			wantStatusCode: http.StatusInternalServerError,
		},
		{
			name:       "ListNodeSets db error returns 500",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID}, nil)
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return(nil, fmt.Errorf("db unavailable"))
			},
			wantStatusCode: http.StatusInternalServerError,
		},
		{
			name:       "empty node sets returns empty changes",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID}, nil)
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				assert.Empty(t, body.Changes)
			},
		},
		{
			name:       "single node set has no changed fields",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID}, nil)
				ns := makeNodeSet(nodeSetID1, t1, nil)
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{ns}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				require.Len(t, body.Changes, 1)
				assert.Equal(t, nodeSetID1, body.Changes[0].NodeSetID)
				assert.Empty(t, body.Changes[0].ChangedFields)
				assert.False(t, body.Changes[0].IsCurrent)
			},
		},
		{
			name:       "isCurrent reflects node pool current node set",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(
					db.NodePool{ID: nodePoolID, CurrentNodeSetID: &nodeSetID1}, nil,
				)
				ns := makeNodeSet(nodeSetID1, t1, nil)
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{ns}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				require.Len(t, body.Changes, 1)
				assert.True(t, body.Changes[0].IsCurrent)
			},
		},
		{
			name:       "osProfile change detected",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(
					db.NodePool{ID: nodePoolID, CurrentNodeSetID: &nodeSetID2}, nil,
				)
				ns1 := makeNodeSet(nodeSetID1, t1, nil)
				ns2 := makeNodeSet(nodeSetID2, t2, func(ns *db.NodeSet) {
					ns.Nico.OSProfile = "nke-260414-24.04-arm64-fde"
				})
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{ns1, ns2}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				require.Len(t, body.Changes, 2)

				// First entry: initial snapshot, no fields
				assert.Empty(t, body.Changes[0].ChangedFields)
				assert.False(t, body.Changes[0].IsCurrent)

				// Second entry: osProfile changed
				require.Len(t, body.Changes[1].ChangedFields, 1)
				f := body.Changes[1].ChangedFields[0]
				assert.Equal(t, "osProfile", f.Field)
				assert.Equal(t, "nke-1.34.2-251201-arm64-ytl", *f.From)
				assert.Equal(t, "nke-260414-24.04-arm64-fde", f.To)
				assert.True(t, body.Changes[1].IsCurrent)
			},
		},
		{
			name:       "multiple fields changed in one snapshot",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID}, nil)
				ns1 := makeNodeSet(nodeSetID1, t1, nil)
				ns2 := makeNodeSet(nodeSetID2, t2, func(ns *db.NodeSet) {
					ns.KubernetesVersion = "1.35.0"
					ns.SystemReservedMemory = "32Gi"
					ns.KubeReservedMemory = "250Mi"
				})
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{ns1, ns2}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				require.Len(t, body.Changes, 2)
				fields := body.Changes[1].ChangedFields
				require.Len(t, fields, 3)
				fieldNames := []string{fields[0].Field, fields[1].Field, fields[2].Field}
				assert.Contains(t, fieldNames, "kubernetesVersion")
				assert.Contains(t, fieldNames, "systemReservedMemory")
				assert.Contains(t, fieldNames, "kubeReservedMemory")
			},
		},
		{
			name:       "node sets returned out of order are sorted by createdAt",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(
					db.NodePool{ID: nodePoolID, CurrentNodeSetID: &nodeSetID3}, nil,
				)
				// Return in reverse order to verify sorting
				ns3 := makeNodeSet(nodeSetID3, t3, func(ns *db.NodeSet) { ns.Nico.OSProfile = "nke-v3" })
				ns1 := makeNodeSet(nodeSetID1, t1, nil)
				ns2 := makeNodeSet(nodeSetID2, t2, func(ns *db.NodeSet) { ns.Nico.OSProfile = "nke-v2" })
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{ns3, ns1, ns2}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				require.Len(t, body.Changes, 3)
				assert.Equal(t, nodeSetID1, body.Changes[0].NodeSetID)
				assert.Equal(t, nodeSetID2, body.Changes[1].NodeSetID)
				assert.Equal(t, nodeSetID3, body.Changes[2].NodeSetID)

				// Only the last is current
				assert.False(t, body.Changes[0].IsCurrent)
				assert.False(t, body.Changes[1].IsCurrent)
				assert.True(t, body.Changes[2].IsCurrent)

				// First entry has no fields, second and third show diffs
				assert.Empty(t, body.Changes[0].ChangedFields)
				require.Len(t, body.Changes[1].ChangedFields, 1)
				assert.Equal(t, "osProfile", body.Changes[1].ChangedFields[0].Field)
				assert.Equal(t, "nke-v2", body.Changes[1].ChangedFields[0].To)
			},
		},
		{
			name:       "unchanged config between snapshots produces no changed fields",
			nodePoolID: nodePoolID,
			setupMock: func(m *testutil.MockDB) {
				m.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID}, nil)
				ns1 := makeNodeSet(nodeSetID1, t1, nil)
				ns2 := makeNodeSet(nodeSetID2, t2, nil) // identical config
				m.On("ListNodeSets", mock.Anything, db.NodeSetFilters{NodePoolID: &nodePoolID}).Return([]db.NodeSet{ns1, ns2}, nil)
			},
			wantStatusCode: http.StatusOK,
			assertBody: func(t *testing.T, res *http.Response) {
				t.Helper()
				var body api.NodePoolConfigHistory
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				require.Len(t, body.Changes, 2)
				assert.Empty(t, body.Changes[1].ChangedFields)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			tc.setupMock(mockDB)

			handler := newTestServerWithValidator(t, mockDB)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
				fmt.Sprintf("/api/nodePools/%s/configHistory", tc.nodePoolID),
				http.NoBody,
			)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			res := w.Result()
			assert.Equal(t, tc.wantStatusCode, res.StatusCode)
			if tc.assertBody != nil {
				tc.assertBody(t, res)
			}

			mockDB.AssertExpectations(t)
		})
	}
}
