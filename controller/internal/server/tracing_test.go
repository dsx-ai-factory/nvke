// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// requestLogs sends one request the API rejects with a 4xx and returns the
// response and the JSON log records the server wrote for it.
func requestLogs(t *testing.T, exportEnabled bool, traceparent, userAgent string) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(otelx.LogHandler{Handler: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})})
	cfg := config.Config{ProfilesDir: "./profiles", Tracing: config.TracingConfig{Enabled: exportEnabled}}
	srv, err := server.NewServer(cfg, logger, &testutil.MockDB{}, nil, nil, nil)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clusters", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	response := httptest.NewRecorder()
	srv.Handler.ServeHTTP(response, req)

	var records []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		records = append(records, record)
	}
	return response, records
}

func createClusterRequestLogs(
	t *testing.T,
	database *testutil.MockDB,
	request api.CreateClusterRequest,
) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(otelx.LogHandler{Handler: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})})
	srv, err := server.NewServer(config.Config{ProfilesDir: "./profiles"}, logger, database, nil, nil, nil)
	require.NoError(t, err)

	body, err := json.Marshal(request)
	require.NoError(t, err)
	httpRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/clusters", bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Handler.ServeHTTP(response, httpRequest)

	var records []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		records = append(records, record)
	}
	return response, records
}

func completedRecord(t *testing.T, records []map[string]any) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["msg"] == "Request completed" {
			return record
		}
	}
	require.Fail(t, "no \"Request completed\" log record", "%v", records)
	return nil
}

// installTracerProvider installs a provider that records spans in memory for
// the duration of the test.
func installTracerProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return recorder
}

func TestRequestCompletedLogCarriesEventAndOutcome(t *testing.T) {
	installTracerProvider(t)

	response, records := requestLogs(t, false, "", "")

	require.GreaterOrEqual(t, response.Code, 400)
	record := completedRecord(t, records)
	require.Equal(t, "http.completed", record["event"])
	require.Equal(t, "rejected", record["outcome"], "a 4xx is rejected, not failed")
	require.EqualValues(t, response.Code, record["status"])
}

func TestRequestCompletedLogCarriesValidationReason(t *testing.T) {
	installTracerProvider(t)

	response, records := requestLogs(t, false, "", "")

	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, "request_validation_failed", completedRecord(t, records)["reason"])
	encodedLogs, err := json.Marshal(records)
	require.NoError(t, err)
	require.NotContains(t, string(encodedLogs), "not json")
}

func TestRequestSpanUsesMatchedRoute(t *testing.T) {
	recorder := installTracerProvider(t)

	response, _ := requestLogs(t, false, "", "")

	require.Equal(t, http.StatusBadRequest, response.Code)
	spanNames := make([]string, 0, len(recorder.Ended()))
	for _, span := range recorder.Ended() {
		spanNames = append(spanNames, span.Name())
	}
	require.Contains(t, spanNames, "POST /api/clusters")
}

func TestVersionProbeDoesNotCreateSpan(t *testing.T) {
	recorder := installTracerProvider(t)

	srv, err := server.NewServer(config.Config{ProfilesDir: "./profiles"}, slog.Default(), &testutil.MockDB{}, nil, nil, nil)
	require.NoError(t, err)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/version", nil)
	response := httptest.NewRecorder()
	srv.Handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Empty(t, recorder.Ended())
}

func TestRequestCompletedLogClassifiesInvalidClusterNetwork(t *testing.T) {
	installTracerProvider(t)
	request := openAPIForgeCreateClusterRequest()
	request.Network = &api.ClusterNetworkCreateConfig{PodCIDR: "not-a-cidr"}

	response, records := createClusterRequestLogs(t, &testutil.MockDB{}, request)

	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, "invalid_cluster_network", completedRecord(t, records)["reason"])
	encodedLogs, err := json.Marshal(records)
	require.NoError(t, err)
	require.NotContains(t, string(encodedLogs), "not-a-cidr")
}

func TestRequestCompletedLogClassifiesDuplicateClusterName(t *testing.T) {
	installTracerProvider(t)
	request := openAPIForgeCreateClusterRequest()
	request.Name = "client-supplied-duplicate-name"
	mockTx := &testutil.MockTx{}
	mockTx.On("CreateCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, db.ErrClusterNameNotUnique).Once()
	database := &testutil.MockDB{MockTx: mockTx}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()

	response, records := createClusterRequestLogs(t, database, request)

	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, "cluster_name_already_exists", completedRecord(t, records)["reason"])
	encodedLogs, err := json.Marshal(records)
	require.NoError(t, err)
	require.NotContains(t, string(encodedLogs), request.Name)
	mockTx.AssertExpectations(t)
	database.AssertExpectations(t)
}

func TestRequestStartsATraceWithoutExport(t *testing.T) {
	installTracerProvider(t)

	response, records := requestLogs(t, false, "", "")

	traceID := response.Header().Get("X-Trace-ID")
	require.Len(t, traceID, 32, "a request with no traceparent starts a trace even with export off")
	require.NotEqual(t, strings.Repeat("0", 32), traceID)
	// The OpenAPI validation lines are logged without the request context, so
	// only the request log pair is checked.
	require.Equal(t, traceID, completedRecord(t, records)["trace_id"])
	for _, record := range records {
		if record["msg"] == "Incoming request" {
			require.Equal(t, traceID, record["trace_id"])
		}
	}
}

func TestRequestTraceIDIsReturnedAndLogged(t *testing.T) {
	installTracerProvider(t)
	const traceID = "11111111111111111111111111111111"

	for _, exportEnabled := range []bool{false, true} {
		response, records := requestLogs(t, exportEnabled, "00-"+traceID+"-2222222222222222-01", "")

		require.Equal(t, traceID, response.Header().Get("X-Trace-ID"), "a valid incoming traceparent is continued (export %t)", exportEnabled)
		record := completedRecord(t, records)
		require.Equal(t, traceID, record["trace_id"])
		require.NotEmpty(t, record["span_id"])
	}
}

func TestRequestLogCarriesUserAgent(t *testing.T) {
	installTracerProvider(t)

	_, records := requestLogs(t, false, "", "nvkectl/v1.2.3")

	for _, record := range records {
		if record["msg"] == "Incoming request" || record["msg"] == "Request completed" {
			require.Equal(t, "nvkectl/v1.2.3", record["user_agent"], "%v", record)
		}
	}
	require.Equal(t, "nvkectl/v1.2.3", completedRecord(t, records)["user_agent"])
}

func TestRequestLogCapsUserAgentLength(t *testing.T) {
	installTracerProvider(t)

	_, records := requestLogs(t, false, "", strings.Repeat("a", 4096))

	userAgent, ok := completedRecord(t, records)["user_agent"].(string)
	require.True(t, ok)
	require.Len(t, userAgent, 256)
}
