// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"context"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Capture persists the W3C trace context of the current span. Arbitrary inbound
// baggage is deliberately not copied into database rows.
func Capture(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier
}

// Restore attaches a persisted trace context to a fresh consumer context,
// preserving its cancellation and deadline rather than the original HTTP
// request lifetime.
func Restore(ctx context.Context, carrier map[string]string) context.Context {
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier(carrier))
}

// ExposeTraceID returns the request's trace ID in the X-Trace-ID response
// header. It runs inside the HTTP tracing middleware and before request logs.
func ExposeTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set("X-Trace-ID", sc.TraceID().String())
		}
		next.ServeHTTP(w, r)
	})
}

// LogHandler enriches context-aware slog calls with correlation fields.
type LogHandler struct{ slog.Handler }

// Handle adds the current span identifiers to a log record.
func (h LogHandler) Handle(ctx context.Context, record slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, record)
}

// WithAttrs preserves enrichment on derived loggers.
func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup preserves enrichment on grouped loggers.
func (h LogHandler) WithGroup(name string) slog.Handler {
	return LogHandler{Handler: h.Handler.WithGroup(name)}
}

// NormalizeLogAttr aligns Temporal SDK correlation fields with HTTP/SQL logs.
func NormalizeLogAttr(_ []string, attr slog.Attr) slog.Attr {
	switch attr.Key {
	case "TraceID":
		attr.Key = "trace_id"
	case "SpanID":
		attr.Key = "span_id"
	case "WorkflowID", "workflowID":
		attr.Key = "workflow_id"
	case "RunID", "runID":
		attr.Key = "run_id"
	case "ActivityID":
		attr.Key = "activity_id"
	case "Attempt":
		attr.Key = "attempt"
	}
	return attr
}
