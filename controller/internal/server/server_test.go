// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/server"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func ptrString(v string) *string { return &v }

// TestOpenAPIValidator_CreateCluster verifies that the OpenAPI request validator
// middleware accepts or rejects create-cluster requests based on request schema.
func TestOpenAPIValidator_CreateCluster(t *testing.T) {
	t.Run("kubernetesVersion", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockDB.MockTx.On("GetSite", mock.Anything, mock.Anything, mock.Anything).Return(
			db.Site{}, db.ErrNotFound,
		).Maybe()

		handler := newTestServerWithValidator(t, mockDB)

		siteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
		vpcID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
		instanceTypeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")
		subnetID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440003")

		baseBody := api.CreateClusterRequest{
			Name:                          "test-cluster",
			OrgID:                         testOrgID,
			SiteID:                        siteID,
			VpcID:                         vpcID,
			CniPlugin:                     "calico",
			EtcdEncrypted:                 false,
			ControlPlaneKubernetesVersion: "1.29.7", // overridden per case
			NodePools: []api.NodePoolCreateConfig{
				{
					Name:             "cp",
					NodeNamePrefix:   "cp-prefix",
					InstanceTypeID:   instanceTypeID,
					OSProfile:        "ubuntu",
					ConfigProfile:    "default",
					ControlPlane:     true,
					DesiredNodeCount: 1,
					SubnetIDs:        []uuid.UUID{subnetID},
				},
			},
		}

		tests := []struct {
			name         string
			version      string
			expectReject bool
		}{
			{"valid version", "1.30.14", false},
			{"valid version alternate", "1.29.10", false},
			{"empty version", "", true},
			{"only major.minor", "1.30", true},
			{"only major", "1", true},
			{"too many parts", "1.30.14.1", true},
			{"no dots", "130", true},
			{"non-numeric major", "a.30.5", true},
			{"non-numeric minor", "1.b.5", true},
			{"non-numeric patch", "1.30.c", true},
			{"leading v", "v1.29.7", true},
			{"leading V uppercase", "V1.29.7", true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				body := baseBody
				body.ControlPlaneKubernetesVersion = tt.version

				bodyBytes, err := json.Marshal(body)
				require.NoError(t, err)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clusters", bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")

				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)

				if tt.expectReject {
					assert.Equal(t, http.StatusBadRequest, rr.Code, "expected middleware to reject invalid version")
					var errBody struct {
						Message string `json:"message"`
					}
					_ = json.Unmarshal(rr.Body.Bytes(), &errBody)
					assert.NotEmpty(t, errBody.Message, "expected validation error message")
				} else {
					bodyStr := rr.Body.String()
					isValidationError := rr.Code == http.StatusBadRequest &&
						(strings.Contains(bodyStr, "pattern") || strings.Contains(bodyStr, "does not match") || strings.Contains(bodyStr, "request body"))
					assert.False(t, isValidationError, "valid version should not be rejected by OpenAPI validator; got %d: %s", rr.Code, bodyStr)
				}
			})
		}
	})

	t.Run("cniVersion", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockDB.MockTx.On("GetSite", mock.Anything, mock.Anything, mock.Anything).Return(
			db.Site{}, db.ErrNotFound,
		).Maybe()

		handler := newTestServerWithValidator(t, mockDB)

		siteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
		vpcID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
		instanceTypeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")
		subnetID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440003")

		baseBody := api.CreateClusterRequest{
			Name:                          "test-cluster",
			OrgID:                         testOrgID,
			SiteID:                        siteID,
			VpcID:                         vpcID,
			CniPlugin:                     "calico",
			EtcdEncrypted:                 false,
			ControlPlaneKubernetesVersion: "1.29.7",
			NodePools: []api.NodePoolCreateConfig{
				{
					Name:             "cp",
					NodeNamePrefix:   "cp-prefix",
					InstanceTypeID:   instanceTypeID,
					OSProfile:        "ubuntu",
					ConfigProfile:    "default",
					ControlPlane:     true,
					DesiredNodeCount: 1,
					SubnetIDs:        []uuid.UUID{subnetID},
				},
			},
		}

		tests := []struct {
			name         string
			cniVersion   *string
			expectReject bool
		}{
			{"omitted version", nil, false},
			{"valid version", ptrString("3.31.4"), false},
			{"package revision", ptrString("3.30.1-1"), true},
			{"leading v", ptrString("v3.31.4"), true},
			{"missing patch", ptrString("3.31"), true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				body := baseBody
				body.CniVersion = tt.cniVersion

				bodyBytes, err := json.Marshal(body)
				require.NoError(t, err)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clusters", bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")

				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)

				if tt.expectReject {
					assert.Equal(t, http.StatusBadRequest, rr.Code, "expected middleware to reject invalid CNI version")
					bodyStr := rr.Body.String()
					assert.True(t,
						strings.Contains(bodyStr, "cniVersion") ||
							strings.Contains(bodyStr, "pattern") ||
							strings.Contains(bodyStr, "does not match") ||
							strings.Contains(bodyStr, "request body"),
						"invalid CNI version rejection should reference the offending field or pattern; got %d: %s", rr.Code, bodyStr)
				} else {
					bodyStr := rr.Body.String()
					isValidationError := rr.Code == http.StatusBadRequest &&
						(strings.Contains(bodyStr, "cniVersion") || strings.Contains(bodyStr, "request body"))
					assert.False(t, isValidationError, "valid CNI version should not be rejected by OpenAPI validator; got %d: %s", rr.Code, bodyStr)
				}
			})
		}
	})

	t.Run("clusterName", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockDB.MockTx.On("GetSite", mock.Anything, mock.Anything, mock.Anything).Return(
			db.Site{}, db.ErrNotFound,
		).Maybe()

		handler := newTestServerWithValidator(t, mockDB)

		siteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
		vpcID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
		instanceTypeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440004")
		subnetID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440003")

		baseBody := api.CreateClusterRequest{
			Name:                          "test-cluster",
			OrgID:                         testOrgID,
			SiteID:                        siteID,
			VpcID:                         vpcID,
			CniPlugin:                     "calico",
			EtcdEncrypted:                 false,
			ControlPlaneKubernetesVersion: "1.29.7",
			NodePools: []api.NodePoolCreateConfig{
				{
					Name:             "cp",
					NodeNamePrefix:   "cp-prefix",
					InstanceTypeID:   instanceTypeID,
					OSProfile:        "ubuntu",
					ConfigProfile:    "default",
					ControlPlane:     true,
					DesiredNodeCount: 1,
					SubnetIDs:        []uuid.UUID{subnetID},
				},
			},
		}

		tests := []struct {
			name         string
			clusterName  string
			expectReject bool
		}{
			{"valid name", "test-cluster", false},
			{"valid max length", strings.Repeat("a", capi.ClusterNameMaxLength), false},
			{"contains dot", "test.cluster", true},
			{"contains uppercase", "Test-cluster", true},
			{"too long for cluster namespace", strings.Repeat("a", capi.ClusterNameMaxLength+1), true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				body := baseBody
				body.Name = tt.clusterName

				bodyBytes, err := json.Marshal(body)
				require.NoError(t, err)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clusters", bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")

				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)

				if tt.expectReject {
					assert.Equal(t, http.StatusBadRequest, rr.Code, "expected middleware to reject invalid cluster name")
					var errBody struct {
						Message string `json:"message"`
					}
					_ = json.Unmarshal(rr.Body.Bytes(), &errBody)
					assert.NotEmpty(t, errBody.Message, "expected validation error message")
				} else {
					bodyStr := rr.Body.String()
					isValidationError := rr.Code == http.StatusBadRequest &&
						(strings.Contains(bodyStr, "pattern") || strings.Contains(bodyStr, "does not match") || strings.Contains(bodyStr, "request body"))
					assert.False(t, isValidationError, "valid cluster name should not be rejected by OpenAPI validator; got %d: %s", rr.Code, bodyStr)
				}
			})
		}
	})
}

// TestOpenAPIValidator_UpdateCluster verifies that the OpenAPI request validator
// middleware accepts or rejects update-cluster (PATCH) requests based on request schema.
func TestOpenAPIValidator_UpdateCluster(t *testing.T) {
	t.Run("rejects read-only network field", func(t *testing.T) {
		handler := newTestServerWithValidator(t, &testutil.MockDB{})
		clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

		req := httptest.NewRequest(
			http.MethodPatch,
			"/api/clusters/"+clusterID.String(),
			bytes.NewReader([]byte(`{"network":{"podCIDR":"10.233.64.0/18"}}`)),
		)
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		var errBody struct {
			Message string `json:"message"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errBody))
		assert.Equal(t, "readOnly property \"network\" in request", errBody.Message)
	})

	t.Run("kubernetesVersion", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, mock.Anything).Return(
			db.Cluster{}, db.ErrNotFound,
		).Maybe()

		handler := newTestServerWithValidator(t, mockDB)
		clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

		tests := []struct {
			name         string
			version      string
			expectReject bool
		}{
			{"valid version", "1.30.14", false},
			{"valid version alternate", "1.29.10", false},
			{"leading v", "v1.29.7", true},
			{"only major.minor", "1.30", true},
			{"empty version", "", true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				version := tt.version
				body := api.UpdateClusterRequest{
					ControlPlaneKubernetesVersion: &version,
				}
				bodyBytes, err := json.Marshal(body)
				require.NoError(t, err)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/api/clusters/"+clusterID.String(), bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")

				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)

				if tt.expectReject {
					assert.Equal(t, http.StatusBadRequest, rr.Code, "expected middleware to reject invalid version")
					var errBody struct {
						Message string `json:"message"`
					}
					_ = json.Unmarshal(rr.Body.Bytes(), &errBody)
					assert.NotEmpty(t, errBody.Message, "expected validation error message")
				} else {
					bodyStr := rr.Body.String()
					isValidationError := rr.Code == http.StatusBadRequest &&
						(strings.Contains(bodyStr, "pattern") || strings.Contains(bodyStr, "does not match") || strings.Contains(bodyStr, "request body"))
					assert.False(t, isValidationError, "valid version should not be rejected by OpenAPI validator; got %d: %s", rr.Code, bodyStr)
				}
			})
		}
	})

	t.Run("accepts enableEtcdEncryption field", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, mock.Anything).Return(
			db.Cluster{}, db.ErrNotFound,
		).Maybe()

		handler := newTestServerWithValidator(t, mockDB)
		clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/api/clusters/"+clusterID.String(), bytes.NewReader([]byte(`{"enableEtcdEncryption":true}`)))
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code, "enableEtcdEncryption should pass OpenAPI validation and reach the handler")
	})
}

// TestStdServerErrorStatus_MalformedPathUUID verifies that requests with a malformed
// UUID path parameter return HTTP 400 (client error) rather than HTTP 500.
func TestStdServerErrorStatus_MalformedPathUUID(t *testing.T) {
	mockDB := &testutil.MockDB{}
	mockDB.MockTx = &testutil.MockTx{}
	mockDB.GetTx = func() db.Tx { return mockDB.MockTx }

	handler := newTestServerWithValidator(t, mockDB)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"DELETE cluster with trailing $", http.MethodDelete, "/api/clusters/a95a73b2-4ebc-4d36-946f-ee88dffe45d2$"},
		{"GET cluster with non-uuid", http.MethodGet, "/api/clusters/not-a-uuid"},
		{"PATCH cluster with non-uuid", http.MethodPatch, "/api/clusters/not-a-uuid"},
		{"GET node pool with non-uuid", http.MethodGet, "/api/nodePools/not-a-uuid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Reader
			if tc.method == http.MethodPatch {
				body = bytes.NewReader([]byte(`{}`))
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, body)
			if tc.method == http.MethodPatch {
				req.Header.Set("Content-Type", "application/json")
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Code, "malformed UUID should return 400, got %d: %s", rr.Code, rr.Body.String())
		})
	}
}

// newTestServerWithValidator builds an HTTP handler with the same OpenAPI validator
// middleware as production. The caller must configure mockDB (GetTx, RunInTx, and
// any tx method mocks) as needed; invalid requests are rejected by the validator.
func newTestServerWithValidator(t *testing.T, mockDB *testutil.MockDB) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Config{
		FWEAPIKey:            "test-key",
		UIDir:                "./ui",
		ManagedByK8sPlatform: false,
		ProfilesDir:          "./profiles",
	}

	forgeProvider := forge.NewForgeProvider(cfg, mockDB, logger)

	srv, err := server.NewServer(cfg, logger, mockDB, forgeProvider, nil)
	require.NoError(t, err)
	require.NotNil(t, srv)

	return srv.Handler
}
