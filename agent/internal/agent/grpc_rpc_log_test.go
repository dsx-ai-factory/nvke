// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

func testTraceContext(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()

	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(context.Background(), spanContext), spanContext
}

func TestLogRPCSpanCarriesTraceContext(t *testing.T) {
	ctx, spanContext := testTraceContext(t)
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	complete := logRPCSpan(ctx, "StreamExecuteScript")
	complete()

	decoder := json.NewDecoder(&output)
	for _, message := range []string{"executing RPC", "RPC execution complete"} {
		var record map[string]any
		require.NoError(t, decoder.Decode(&record))
		assert.Equal(t, message, record["msg"])
		assert.Equal(t, "StreamExecuteScript", record["rpc"])
		assert.Equal(t, spanContext.TraceID().String(), record["trace_id"])
		assert.Equal(t, spanContext.SpanID().String(), record["span_id"])
	}
}

func TestLogScriptExecutionCarriesTraceContext(t *testing.T) {
	ctx, spanContext := testTraceContext(t)
	var output bytes.Buffer
	server := NodeAgentServer{ScriptExecutionLogger: slog.New(slog.NewJSONHandler(&output, nil))}

	server.logScriptExecution(ctx, "StreamExecuteScript", "request-1", &pb.ExecuteScriptRequest{
		ScriptName: "example.sh",
	}, SuccessStatus, 0, "", "complete")

	var record map[string]any
	require.NoError(t, json.NewDecoder(&output).Decode(&record))
	assert.Equal(t, "StreamExecuteScript", record["msg"])
	assert.Equal(t, "request-1", record["request_id"])
	assert.Equal(t, spanContext.TraceID().String(), record["trace_id"])
	assert.Equal(t, spanContext.SpanID().String(), record["span_id"])

	server.logScriptExecution(context.Background(), "ExecuteScript", "request-2", &pb.ExecuteScriptRequest{
		ScriptName: "example.sh",
	}, SuccessStatus, 0, "", "complete")

	var untracedRecord map[string]any
	require.NoError(t, json.NewDecoder(&output).Decode(&untracedRecord))
	assert.Equal(t, "request-2", untracedRecord["request_id"])
	assert.NotContains(t, untracedRecord, "trace_id")
	assert.NotContains(t, untracedRecord, "span_id")
}

func TestTraceLogAttrsOmitsInvalidContext(t *testing.T) {
	assert.Empty(t, traceLogAttrs(context.Background()))
}
