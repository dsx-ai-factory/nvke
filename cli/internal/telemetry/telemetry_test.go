// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// These tests install a process-wide tracer provider through Run.Start, so
// none of them is parallel.

var traceparentRE = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`)

func newTestRun(t *testing.T) (*Run, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	return NewRun("v1.2.3", WithSpanProcessor(rec)), rec
}

func TestTransportInjectsTraceparentAndUserAgent(t *testing.T) {
	run, rec := newTestRun(t)
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	ctx := run.Start(context.Background(), "nvkectl get clusters")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := (&http.Client{Transport: Transport(nil)}).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	run.Finish(nil)

	m := traceparentRE.FindStringSubmatch(got.Get("Traceparent"))
	require.NotNil(t, m, "traceparent %q", got.Get("Traceparent"))
	assert.Equal(t, run.TraceID(), m[1], "traceparent carries the run's trace ID")
	assert.Equal(t, "nvkectl/v1.2.3", got.Get("User-Agent"))
	assert.Equal(t, "Bearer secret", got.Get("Authorization"), "other headers pass through")

	spans := rec.Ended()
	require.Len(t, spans, 2, "the request span and the root span")
	for _, s := range spans {
		assert.Equal(t, m[1], s.SpanContext().TraceID().String())
	}
}

func TestRootSpanRecordsOutcome(t *testing.T) {
	run, rec := newTestRun(t)
	run.Start(context.Background(), "nvkectl create cluster")
	run.Finish(errors.New("boom"))

	spans := rec.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "nvkectl create cluster", spans[0].Name())
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.False(t, spans[0].Parent().IsValid(), "the command span is the root")
}

func TestRunWithoutStart(t *testing.T) {
	run := NewRun("dev")
	assert.Empty(t, run.TraceID())
	run.Finish(nil) // must not panic
	assert.Empty(t, (*Run)(nil).TraceID(), "a nil Run has no trace")
	inner := slog.NewTextHandler(&bytes.Buffer{}, nil)
	assert.Same(t, inner, run.LogHandler(inner))
}

func TestStartTwiceKeepsOneTrace(t *testing.T) {
	run, _ := newTestRun(t)
	first := run.Start(context.Background(), "a")
	second := run.Start(context.Background(), "b")
	assert.Equal(t, trace.SpanContextFromContext(first).TraceID(), trace.SpanContextFromContext(second).TraceID())
	assert.Equal(t, run.TraceID(), trace.SpanContextFromContext(first).TraceID().String())
	assert.Len(t, run.TraceID(), 32)
}

func TestLogHandlerAddsTraceFields(t *testing.T) {
	run, _ := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl get clusters")
	var buf bytes.Buffer
	logger := slog.New(run.LogHandler(slog.NewJSONHandler(&buf, nil))).With("component", "x")

	logger.Info("without context")
	logger.InfoContext(ctx, "with context")

	dec := json.NewDecoder(&buf)
	for range 2 {
		var rec map[string]any
		require.NoError(t, dec.Decode(&rec))
		assert.Equal(t, run.TraceID(), rec["trace_id"])
		assert.Equal(t, run.SpanContext().SpanID().String(), rec["span_id"])
		assert.Equal(t, "x", rec["component"], "WithAttrs keeps the wrapper")
	}
}
