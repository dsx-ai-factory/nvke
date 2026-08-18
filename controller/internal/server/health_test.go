// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestHealthzAlwaysHealthyWithoutDependencyChecks(t *testing.T) {
	var calls atomic.Int32
	hc := testHealthChecker([]dependencyCheck{{
		name: "unused",
		check: func(context.Context) error {
			calls.Add(1)
			return errors.New("must not run")
		},
	}})

	rr := httptest.NewRecorder()
	hc.healthz(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, healthz, http.NoBody))

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"ok"}`, rr.Body.String())
	assert.Zero(t, calls.Load())
}

func TestReadyzReportsDependencyResults(t *testing.T) {
	tests := []struct {
		name           string
		checks         []dependencyCheck
		expectedStatus int
		expectedBody   string
	}{
		{
			name: "all checks pass",
			checks: []dependencyCheck{
				{name: databaseConst, check: func(context.Context) error { return nil }},
				{name: temporal, check: func(context.Context) error { return nil }},
			},
			expectedStatus: http.StatusOK,
			expectedBody:   `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`,
		},
		{
			name: "one check fails",
			checks: []dependencyCheck{
				{name: databaseConst, check: func(context.Context) error { return nil }},
				{name: temporal, check: func(context.Context) error { return errors.New("frontend unavailable") }},
			},
			expectedStatus: http.StatusServiceUnavailable,
			expectedBody:   `{"status":"unavailable","checks":{"database":"ok","temporal":"frontend unavailable"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := testHealthChecker(tt.checks)
			rr := httptest.NewRecorder()

			hc.readyz(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))

			assert.Equal(t, tt.expectedStatus, rr.Code)
			assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
			assert.JSONEq(t, tt.expectedBody, rr.Body.String())
		})
	}
}

func TestReadyzTimesOutDependencyCheck(t *testing.T) {
	const checkTimeout = 50 * time.Millisecond
	hc := testHealthChecker([]dependencyCheck{{
		name: "blocked",
		check: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}})
	hc.checkTimeout = checkTimeout
	rr := httptest.NewRecorder()

	started := time.Now()
	hc.readyz(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
	elapsed := time.Since(started)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Less(t, elapsed, 2*checkTimeout)
	assert.JSONEq(t, `{"status":"unavailable","checks":{"blocked":"context deadline exceeded"}}`, rr.Body.String())
}

func TestReadyzCachesCheckResultUntilTTLExpires(t *testing.T) {
	const cacheTTL = 30 * time.Millisecond
	var calls atomic.Int32
	hc := testHealthChecker([]dependencyCheck{{
		name: "counted",
		check: func(context.Context) error {
			calls.Add(1)
			return nil
		},
	}})
	hc.cacheTTL = cacheTTL

	for range 3 {
		rr := httptest.NewRecorder()
		hc.readyz(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
		require.Equal(t, http.StatusOK, rr.Code)
	}
	assert.Equal(t, int32(1), calls.Load())

	time.Sleep(2 * cacheTTL)
	rr := httptest.NewRecorder()
	hc.readyz(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, int32(2), calls.Load())
}

func TestServerHealthEndpoints(t *testing.T) {
	logger := testHealthLogger()
	mockDB := &testutil.MockDB{}
	mockDB.On("Ping", mock.Anything).Return(nil).Once()
	cfg := config.Config{
		FWEAPIKey:            "test-key",
		UIDir:                "./ui",
		ManagedByK8sPlatform: false,
		ProfilesDir:          "./profiles",
	}
	forgeProvider := forge.NewForgeProvider(cfg, mockDB, logger)
	srv, err := NewServer(cfg, logger, mockDB, forgeProvider, nil)
	require.NoError(t, err)

	healthRR := httptest.NewRecorder()
	srv.Handler.ServeHTTP(healthRR, httptest.NewRequestWithContext(t.Context(), http.MethodGet, healthz, http.NoBody))
	assert.Equal(t, http.StatusOK, healthRR.Code)
	assert.JSONEq(t, `{"status":"ok"}`, healthRR.Body.String())

	readyRR := httptest.NewRecorder()
	srv.Handler.ServeHTTP(readyRR, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
	assert.Equal(t, http.StatusServiceUnavailable, readyRR.Code)
	assert.JSONEq(t,
		`{"status":"unavailable","checks":{"database":"ok","temporal":"temporal client is not configured"}}`,
		readyRR.Body.String(),
	)
	mockDB.AssertExpectations(t)
}

func testHealthChecker(checks []dependencyCheck) *healthChecker {
	return &healthChecker{
		logger:       testHealthLogger(),
		checks:       checks,
		checkTimeout: time.Second,
		cacheTTL:     time.Minute,
	}
}

func testHealthLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
