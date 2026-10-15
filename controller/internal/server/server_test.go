// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func ptrString(v string) *string { return &v }

// openAPIForgeCreateClusterRequest returns a Forge create-cluster body that
// passes OpenAPI validation, so each validator case can vary one field and
// attribute any rejection to that field.
func openAPIForgeCreateClusterRequest() api.CreateClusterRequest {
	return api.CreateClusterRequest{
		Name: "test-cluster",
		Nico: &api.NicoClusterProvider{
			OrgID:  testOrgID,
			SiteID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
			VpcID:  uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"),
		},
		CniPlugin:                     "ovn-kubernetes",
		EtcdEncrypted:                 false,
		ControlPlaneKubernetesVersion: "1.29.7",
		ControlPlaneConfig:            nicoCreateControlPlaneConfig(1, false),
	}
}

// stubOpenAPICreateClusterTx stubs the tx methods a forge create reaches after
// OpenAPI validation. Forge network CIDR overlap is checked in forge-console
// now, so valid requests proceed into RunInTx and need these stubs.
func stubOpenAPICreateClusterTx(mockDB *testutil.MockDB) {
	mockDB.MockTx.On("CreateCluster", mock.Anything, mock.Anything).Return(
		db.Cluster{ID: uuid.New(), Name: "test-cluster", KubernetesVersion: "1.29.7"},
		nil,
	).Maybe()
	mockDB.MockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.Anything).Return(
		db.ControlPlaneConfiguration{ID: uuid.New()},
		nil,
	).Maybe()
	mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, nil).Maybe()
	mockDB.MockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		db.WorkflowOutbox{},
		nil,
	).Maybe()
}

// TestOpenAPIValidator_CreateCluster verifies that the OpenAPI request validator
// middleware accepts or rejects create-cluster requests based on request schema.
func TestOpenAPIValidator_CreateCluster(t *testing.T) {
	t.Run("kubernetesVersion", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		stubOpenAPICreateClusterTx(mockDB)

		handler := newTestServerWithValidator(t, mockDB)

		// ControlPlaneKubernetesVersion is overridden per case.
		baseBody := openAPIForgeCreateClusterRequest()

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
		stubOpenAPICreateClusterTx(mockDB)

		handler := newTestServerWithValidator(t, mockDB)

		baseBody := openAPIForgeCreateClusterRequest()

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
		stubOpenAPICreateClusterTx(mockDB)

		handler := newTestServerWithValidator(t, mockDB)

		baseBody := openAPIForgeCreateClusterRequest()

		tests := []struct {
			name         string
			clusterName  string
			expectReject bool
		}{
			{"valid name", "test-cluster", false},
			{"valid max length", strings.Repeat("a", capi.ClusterNameMaxLength), false},
			{"contains dot", "test.cluster", true},
			{"contains uppercase", "Test-cluster", true},
			{"over max length", strings.Repeat("a", capi.ClusterNameMaxLength+1), true},
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

	t.Run("controlPlaneVIP", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		stubOpenAPICreateClusterTx(mockDB)

		handler := newTestServerWithValidator(t, mockDB)

		baseBody := openAPIForgeCreateClusterRequest()

		tests := []struct {
			name         string
			vip          string
			expectReject bool
		}{
			{"omitted VIP", "", false},
			{"valid VIP", "172.31.0.10", false},
			{"invalid VIP", "not-an-ip", true},
			{"IPv6 VIP", "2001:db8::1", true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				body := baseBody
				body.ControlPlaneVIP = tt.vip

				bodyBytes, err := json.Marshal(body)
				require.NoError(t, err)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clusters", bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")

				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)

				if tt.expectReject {
					assert.Equal(t, http.StatusBadRequest, rr.Code, "expected middleware to reject invalid controlPlaneVIP")
					bodyStr := rr.Body.String()
					assert.True(t,
						strings.Contains(bodyStr, "controlPlaneVIP") ||
							strings.Contains(bodyStr, "format") ||
							strings.Contains(bodyStr, "request body"),
						"invalid controlPlaneVIP rejection should reference the offending field or format; got %d: %s", rr.Code, bodyStr)
				} else {
					bodyStr := rr.Body.String()
					isValidationError := rr.Code == http.StatusBadRequest &&
						(strings.Contains(bodyStr, "controlPlaneVIP") || strings.Contains(bodyStr, "request body"))
					assert.False(t, isValidationError, "valid controlPlaneVIP should not be rejected by OpenAPI validator; got %d: %s", rr.Code, bodyStr)
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

		req := httptest.NewRequestWithContext(t.Context(),
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

// TestOpenAPIValidator_UpdateNodePoolRejectsUnknownFields verifies that node-pool
// updates reject fields that are not part of the OpenAPI request schema.
func TestOpenAPIValidator_UpdateNodePoolRejectsUnknownFields(t *testing.T) {
	handler := newTestServerWithValidator(t, &testutil.MockDB{})
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPatch,
		"/api/nodePools/"+nodePoolID.String(),
		bytes.NewReader([]byte(`{"desiredNodeCounnt":2}`)),
	)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var errBody api.GenericError
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Message, "desiredNodeCounnt")
}

// TestOpenAPIValidator_CreateClusterControlPlaneConfig verifies that the control
// plane create schema carries only the settable Nico fields. diskSelectionStrategy
// is reported on read but is not an input in either direction, so it is absent from
// NicoControlPlaneProviderCreate and the validator rejects it before the handler runs.
func TestOpenAPIValidator_CreateClusterControlPlaneConfig(t *testing.T) {
	body := func(nico string) []byte {
		return []byte(`{
			"name": "test-cluster",
			"nico": {
				"orgID": "` + testOrgID + `",
				"siteID": "550e8400-e29b-41d4-a716-446655440001",
				"vpcID": "550e8400-e29b-41d4-a716-446655440002"
			},
			"cniPlugin": "ovn-kubernetes",
			"controlPlaneKubernetesVersion": "1.29.7",
			"etcdEncrypted": false,
			"controlPlaneConfig": {"desiredCount": 3, "nico": ` + nico + `}
		}`)
	}

	tests := []struct {
		name         string
		nico         string
		expectReject bool
	}{
		{
			name: "settable nested fields are accepted",
			nico: `{"release":"ubuntu","configProfile":"default"}`,
		},
		{
			name:         "nico.diskSelectionStrategy is rejected",
			nico:         `{"diskSelectionStrategy":"largest"}`,
			expectReject: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodPost, "/api/clusters", bytes.NewReader(body(tt.nico)))
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			newTestServerWithValidator(t, &testutil.MockDB{}).ServeHTTP(rr, req)

			// The accepted case may still fail in the handler; assert on the message
			// rather than the status code.
			if tt.expectReject {
				assert.Contains(t, rr.Body.String(), "diskSelectionStrategy")
				return
			}
			assert.NotContains(t, rr.Body.String(), "diskSelectionStrategy",
				"settable fields should pass OpenAPI validation")
		})
	}
}

// TestOpenAPIValidator_CreateRequiresExactlyOneProvider verifies that the create
// schemas require exactly one provider shape, so a handler never sees an ambiguous
// provider. CreateClusterRequest, CreateClusterControlPlaneConfig and
// NodePoolCreateConfig each reject a body carrying both nico and capi as well as
// one carrying neither. A cluster create exercises all three, because it embeds
// the control plane config and the initial node pools.
func TestOpenAPIValidator_CreateRequiresExactlyOneProvider(t *testing.T) {
	const (
		clusterNico = `"nico":{"orgID":"` + testOrgID + `",` +
			`"siteID":"550e8400-e29b-41d4-a716-446655440001",` +
			`"vpcID":"550e8400-e29b-41d4-a716-446655440002"}`
		clusterInfra = `"infra":{"cluster":{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1",` +
			`"kind":"DockerCluster","metadata":{"name":"test-cluster"}}}`
		cpNico = `"nico":{"instanceTypePreference":["550e8400-e29b-41d4-a716-446655440003"],` +
			`"release":"ubuntu","configProfile":"default"}`
		cpInfra = `"infra":{"machineTemplate":{` +
			`"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1",` +
			`"kind":"DockerMachineTemplate","metadata":{"name":"cp"}}}`
		poolNico = `"nico":{"instanceTypeID":"550e8400-e29b-41d4-a716-446655440004",` +
			`"release":"ubuntu","configProfile":"default"}`
		poolInfra = `"infra":{"machineTemplate":{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1",` +
			`"kind":"DockerMachineTemplate","metadata":{"name":"worker"}}}`
	)

	// Provider fragments are appended, so an empty fragment leaves the enclosing
	// object valid apart from the missing provider.
	field := func(fragment string) string {
		if fragment == "" {
			return ""
		}
		return "," + fragment
	}
	body := func(cluster, controlPlane, nodePool string) []byte {
		return []byte(`{
			"name": "test-cluster",
			"cniPlugin": "ovn-kubernetes",
			"controlPlaneKubernetesVersion": "1.29.7",
			"etcdEncrypted": false` + field(cluster) + `,
			"controlPlaneConfig": {"desiredCount": 3` + field(controlPlane) + `},
			"nodePools": [
				{"name":"worker","nodeNamePrefix":"worker","desiredNodeCount":1` + field(nodePool) + `}
			]
		}`)
	}

	tests := []struct {
		name         string
		cluster      string
		controlPlane string
		nodePool     string
		expectReject bool
	}{
		{
			name:         "cluster carrying both providers",
			cluster:      clusterNico + "," + clusterInfra,
			controlPlane: cpNico,
			nodePool:     poolNico,
			expectReject: true,
		},
		{
			name:         "cluster carrying neither provider",
			controlPlane: cpNico,
			nodePool:     poolNico,
			expectReject: true,
		},
		{
			name:         "control plane config carrying both providers",
			cluster:      clusterNico,
			controlPlane: cpNico + "," + cpInfra,
			nodePool:     poolNico,
			expectReject: true,
		},
		{
			name:         "control plane config carrying neither provider",
			cluster:      clusterNico,
			nodePool:     poolNico,
			expectReject: true,
		},
		{
			name:         "node pool carrying both providers",
			cluster:      clusterNico,
			controlPlane: cpNico,
			nodePool:     poolNico + "," + poolInfra,
			expectReject: true,
		},
		{
			name:         "node pool carrying neither provider",
			cluster:      clusterNico,
			controlPlane: cpNico,
			expectReject: true,
		},
		{
			name:         "one provider shape per object is accepted",
			cluster:      clusterNico,
			controlPlane: cpNico,
			nodePool:     poolNico,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodPost, "/api/clusters",
				bytes.NewReader(body(tt.cluster, tt.controlPlane, tt.nodePool)))
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			newTestServerWithValidator(t, &testutil.MockDB{}).ServeHTTP(rr, req)

			// Distinguish a schema rejection from a handler rejection by the validator's message.
			if tt.expectReject {
				assert.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				assert.Contains(t, rr.Body.String(), "request body has an error",
					"should be rejected by schema validation rather than by the handler")
				return
			}
			assert.NotContains(t, rr.Body.String(), "request body has an error",
				"exactly one provider shape per object should pass schema validation")
		})
	}
}

// TestOpenAPIValidator_UpdateClusterControlPlaneConfig verifies that the control
// plane update schema carries only the configurable Nico fields. The control plane
// always installs to the smallest disk, so diskSelectionStrategy is absent from
// NicoControlPlaneProviderUpdate and the validator rejects it.
func TestOpenAPIValidator_UpdateClusterControlPlaneConfig(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

	tests := []struct {
		name         string
		body         string
		expectReject bool
	}{
		{
			name:         "configurable nested fields are accepted",
			body:         `{"controlPlaneConfig":{"nico":{"release":"ubuntu","configProfile":"default"}}}`,
			expectReject: false,
		},
		{
			name:         "nico.diskSelectionStrategy is rejected",
			body:         `{"controlPlaneConfig":{"nico":{"diskSelectionStrategy":"largest"}}}`,
			expectReject: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			mockDB.MockTx = &testutil.MockTx{}
			mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound).Maybe()

			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodPatch,
				"/api/clusters/"+clusterID.String(),
				bytes.NewReader([]byte(tt.body)),
			)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			newTestServerWithValidator(t, mockDB).ServeHTTP(rr, req)

			if tt.expectReject {
				assert.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				return
			}
			assert.Equal(t, http.StatusNotFound, rr.Code,
				"should pass OpenAPI validation and reach the handler, body: %s", rr.Body.String())
		})
	}
}

// TestOpenAPIValidator_UpdateNodePool verifies that the update-node-pool schema
// accepts only the mutable subset of the nested nico fields. The fields that are
// fixed at creation are absent from NicoNodePoolProviderUpdate, so the validator
// rejects them before the handler runs; nothing in the service layer re-checks them.
func TestOpenAPIValidator_UpdateNodePool(t *testing.T) {
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440010")

	newHandler := func(t *testing.T) http.Handler {
		t.Helper()
		mockDB := &testutil.MockDB{}
		mockDB.MockTx = &testutil.MockTx{}
		mockDB.GetTx = func() db.Tx { return mockDB.MockTx }
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockDB.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound).Maybe()
		mockDB.MockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound).Maybe()
		return newTestServerWithValidator(t, mockDB)
	}

	tests := []struct {
		name         string
		body         string
		expectReject bool
	}{
		{
			name:         "mutable nested fields are accepted",
			body:         `{"nico":{"subnetIDs":["550e8400-e29b-41d4-a716-446655440001"],"release":"ubuntu","configProfile":"default"}}`,
			expectReject: false,
		},
		{
			name:         "nico.instanceTypeID is rejected",
			body:         `{"nico":{"instanceTypeID":"550e8400-e29b-41d4-a716-446655440002"}}`,
			expectReject: true,
		},
		{
			name:         "nico.infinibandPartitionID is rejected",
			body:         `{"nico":{"infinibandPartitionID":"550e8400-e29b-41d4-a716-446655440003"}}`,
			expectReject: true,
		},
		{
			name:         "nico.nvLinkLogicalPartitionID is rejected",
			body:         `{"nico":{"nvLinkLogicalPartitionID":"550e8400-e29b-41d4-a716-446655440004"}}`,
			expectReject: true,
		},
		{
			name:         "nico.sshKeyGroupIDs is rejected",
			body:         `{"nico":{"sshKeyGroupIDs":["550e8400-e29b-41d4-a716-446655440005"]}}`,
			expectReject: true,
		},
		{
			name:         "a full nico object echoed from GET is rejected",
			body:         `{"nico":{"subnetIDs":[],"vpcPrefixIDs":[],"instanceTypeID":"550e8400-e29b-41d4-a716-446655440002","sshKeyGroupIDs":[],"release":"ubuntu","configProfile":"default"}}`,
			expectReject: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodPatch,
				"/api/nodePools/"+nodePoolID.String(),
				bytes.NewReader([]byte(tt.body)),
			)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			newHandler(t).ServeHTTP(rr, req)

			if tt.expectReject {
				assert.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				return
			}
			assert.Equal(t, http.StatusNotFound, rr.Code,
				"should pass OpenAPI validation and reach the handler, body: %s", rr.Body.String())
		})
	}
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
// When MockTx is set, CreateCluster path stubs are installed so valid forge
// creates (no longer blocked by controller Forge CIDR overlap checks) do not panic.
func newTestServerWithValidator(t *testing.T, mockDB *testutil.MockDB) http.Handler {
	t.Helper()

	if mockDB != nil && mockDB.MockTx != nil {
		stubOpenAPICreateClusterTx(mockDB)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Config{
		ManagedByK8sPlatform: false,
		ProfilesDir:          "./profiles",
	}

	srv, err := server.NewServer(cfg, logger, mockDB, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, srv)

	return srv.Handler
}
