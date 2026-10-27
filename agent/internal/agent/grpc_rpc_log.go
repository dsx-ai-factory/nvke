// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

func traceLogAttrs(ctx context.Context) []slog.Attr {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", spanContext.TraceID().String()),
		slog.String("span_id", spanContext.SpanID().String()),
	}
}

// logRPCSpan logs "executing RPC" and returns a function to defer for "RPC execution complete"
// (runs when the handler returns, after any later-registered defers).
func logRPCSpan(ctx context.Context, rpc string) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	attrs := append([]slog.Attr{slog.String("rpc", rpc)}, traceLogAttrs(ctx)...)
	slog.LogAttrs(ctx, slog.LevelInfo, "executing RPC", attrs...)
	return func() {
		slog.LogAttrs(ctx, slog.LevelInfo, "RPC execution complete", attrs...)
	}
}
