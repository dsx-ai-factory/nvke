// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestCorrelationSurvivesRequestLifetime(t *testing.T) {
	t.Parallel()
	provider := sdktrace.NewTracerProvider()
	defer func() { require.NoError(t, provider.Shutdown(context.Background())) }()
	var carrier map[string]string
	var requestSpan trace.SpanContext
	var logs bytes.Buffer
	logger := slog.New(LogHandler{Handler: slog.NewJSONHandler(&logs, nil)}).With("component", "test")
	handler := otelhttp.NewHandler(ExposeTraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSpan = trace.SpanContextFromContext(r.Context())
		carrier = Capture(r.Context())
		logger.InfoContext(r.Context(), "accepted")
		w.WriteHeader(http.StatusAccepted)
	})), "request", otelhttp.WithTracerProvider(provider), otelhttp.WithPropagators(propagation.TraceContext{}))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	req.Header.Set("tracestate", "vendor=value")
	req.Header.Set("baggage", "secret=must-not-persist")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	cancel()

	consumerCtx, stopConsumer := context.WithCancel(context.Background())
	defer stopConsumer()
	restored := Restore(consumerCtx, carrier)
	require.NoError(t, restored.Err())
	sc := trace.SpanContextFromContext(restored)
	require.Equal(t, requestSpan.TraceID(), sc.TraceID())
	require.Equal(t, requestSpan.SpanID(), sc.SpanID())
	require.Equal(t, "vendor=value", sc.TraceState().String())
	require.NotContains(t, carrier, "baggage")
	require.Equal(t, sc.TraceID().String(), response.Header().Get("X-Trace-ID"))
	var record map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
	require.Equal(t, sc.TraceID().String(), record["trace_id"])
	stopConsumer()
	require.ErrorIs(t, restored.Err(), context.Canceled)
}

func TestCorrelationMissingAndInvalidParent(t *testing.T) {
	t.Parallel()
	provider := sdktrace.NewTracerProvider()
	defer func() { require.NoError(t, provider.Shutdown(context.Background())) }()
	for _, parent := range []string{"", "invalid", "00-00000000000000000000000000000000-2222222222222222-01"} {
		var valid bool
		handler := otelhttp.NewHandler(ExposeTraceID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			valid = trace.SpanContextFromContext(r.Context()).IsValid()
		})), "request", otelhttp.WithTracerProvider(provider), otelhttp.WithPropagators(propagation.TraceContext{}))
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", http.NoBody)
		req.Header.Set("traceparent", parent)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		require.True(t, valid)
		require.Len(t, response.Header().Get("X-Trace-ID"), 32)
	}
	// Rows written before the migration carry no correlation and remain readable.
	require.False(t, trace.SpanContextFromContext(Restore(context.Background(), nil)).IsValid())
}

func TestNormalizeLogAttr(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"TraceID":      "trace_id",
		"SpanID":       "span_id",
		"WorkflowID":   "workflow_id",
		"workflowID":   "workflow_id",
		"RunID":        "run_id",
		"runID":        "run_id",
		"ActivityID":   "activity_id",
		"Attempt":      "attempt",
		"workflowType": "workflowType",
		"msg":          "msg",
		"trace_id":     "trace_id",
	}
	for key, want := range tests {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			got := NormalizeLogAttr(nil, slog.String(key, "value"))
			require.Equal(t, want, got.Key)
			require.Equal(t, "value", got.Value.String())
		})
	}
}
