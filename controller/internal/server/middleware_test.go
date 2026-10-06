// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsExemptPath(t *testing.T) {
	tests := []struct {
		path   string
		exempt bool
	}{
		{"/api/nodes", true},
		{"/api/nodes/550e8400-e29b-41d4-a716-446655440000", true},
		{"/api/nodePools/550e8400-e29b-41d4-a716-446655440000", false},
		{"/api/nodesBackup", false},
		{"/api/clusters", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.exempt, isExemptPath(tt.path), "isExemptPath(%q)", tt.path)
		})
	}
}

func TestIsMutatingMethod(t *testing.T) {
	tests := []struct {
		method   string
		mutating bool
	}{
		{http.MethodPost, true},
		{http.MethodPut, true},
		{http.MethodPatch, true},
		{http.MethodDelete, true},
		{http.MethodGet, false},
		{http.MethodHead, false},
		{http.MethodOptions, false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			assert.Equal(t, tt.mutating, isMutatingMethod(tt.method), "isMutatingMethod(%q)", tt.method)
		})
	}
}

func TestIsOperationalPath(t *testing.T) {
	tests := []struct {
		path        string
		operational bool
	}{
		{healthz, true},
		{"/healthz/live", true},
		{"/readyz", true},
		{metrics, true},
		{"/metrics/cadvisor", true},
		{"/health", false},
		{"/healthzBackup", false},
		{"/readyzReport", false},
		{"/metricsAdmin", false},
		{"/api/nodes", false},
		{"/api/clusters", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.operational, isOperationalPath(tt.path), "isOperationalPath(%q)", tt.path)
		})
	}
}

func TestManagedByMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create a simple test handler that always returns 200 OK
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("success")); err != nil {
			t.Errorf("Failed to write response: %v", err)
		}
	})

	tests := []struct {
		name               string
		enforce            bool
		method             string
		path               string
		managedByHeader    string
		expectedStatusCode int
	}{
		{
			name:               "disabled mode - POST allowed without header",
			enforce:            false,
			method:             http.MethodPost,
			path:               "/api/clusters",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - POST blocked without header",
			enforce:            true,
			method:             http.MethodPost,
			path:               "/api/clusters",
			managedByHeader:    "",
			expectedStatusCode: http.StatusConflict,
		},
		{
			name:               "enforce mode - POST allowed with correct header",
			enforce:            true,
			method:             http.MethodPost,
			path:               "/api/clusters",
			managedByHeader:    PlatformAPIValue,
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - DELETE blocked without header",
			enforce:            true,
			method:             http.MethodDelete,
			path:               "/api/clusters/123",
			managedByHeader:    "",
			expectedStatusCode: http.StatusConflict,
		},
		{
			name:               "enforce mode - DELETE allowed with header",
			enforce:            true,
			method:             http.MethodDelete,
			path:               "/api/clusters/123",
			managedByHeader:    PlatformAPIValue,
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - PUT blocked without header",
			enforce:            true,
			method:             http.MethodPut,
			path:               "/api/clusters/123",
			managedByHeader:    "",
			expectedStatusCode: http.StatusConflict,
		},
		{
			name:               "enforce mode - PATCH blocked without header",
			enforce:            true,
			method:             http.MethodPatch,
			path:               "/api/clusters/123",
			managedByHeader:    "",
			expectedStatusCode: http.StatusConflict,
		},
		{
			name:               "enforce mode - GET allowed without header",
			enforce:            true,
			method:             http.MethodGet,
			path:               "/api/clusters",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - health check allowed without header",
			enforce:            true,
			method:             http.MethodGet,
			path:               healthz,
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - metrics allowed without header",
			enforce:            true,
			method:             http.MethodGet,
			path:               metrics,
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - POST with non-standard header value",
			enforce:            true,
			method:             http.MethodPost,
			path:               "/api/clusters",
			managedByHeader:    "some-other-system",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "disabled mode - DELETE allowed without header",
			enforce:            false,
			method:             http.MethodDelete,
			path:               "/api/clusters/123",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - DELETE node with platform header allowed",
			enforce:            true,
			method:             http.MethodDelete,
			path:               "/api/nodes/550e8400-e29b-41d4-a716-446655440000",
			managedByHeader:    PlatformAPIValue,
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - DELETE node allowed without header",
			enforce:            true,
			method:             http.MethodDelete,
			path:               "/api/nodes/550e8400-e29b-41d4-a716-446655440000",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - DELETE under /api/nodes prefix allowed without header",
			enforce:            true,
			method:             http.MethodDelete,
			path:               "/api/nodes/550e8400-e29b-41d4-a716-446655440000/extra",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - DELETE /api/nodes collection allowed without header",
			enforce:            true,
			method:             http.MethodDelete,
			path:               "/api/nodes",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - POST /api/nodes allowed without header",
			enforce:            true,
			method:             http.MethodPost,
			path:               "/api/nodes",
			managedByHeader:    "",
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "enforce mode - mutating /api/nodePools still requires header",
			enforce:            true,
			method:             http.MethodPatch,
			path:               "/api/nodePools/550e8400-e29b-41d4-a716-446655440000",
			managedByHeader:    "",
			expectedStatusCode: http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create middleware with test configuration
			middleware := managedByMiddleware(tt.enforce, logger)
			handler := middleware(testHandler)

			// Create test request
			req := httptest.NewRequestWithContext(context.Background(), tt.method, tt.path, http.NoBody)
			if tt.managedByHeader != "" {
				req.Header.Set(ManagedByHeader, tt.managedByHeader)
			}

			// Record response
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			// Assert status code
			assert.Equal(t, tt.expectedStatusCode, rr.Code,
				"unexpected status code for %s %s", tt.method, tt.path)

			// Verify response body for blocked requests
			if rr.Code == http.StatusConflict {
				assert.Contains(t, rr.Body.String(), "K8s Platform API",
					"error message should mention K8s Platform API")
				assert.Contains(t, rr.Body.String(), ManagedByHeader,
					"error message should mention the required header")
			}
		})
	}
}
