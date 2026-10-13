// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler adds trace_id and span_id to every record.
type logHandler struct {
	slog.Handler
	root trace.SpanContext
}

// LogHandler wraps inner so each record carries trace_id and span_id: those of
// the span in the record's context, or else those of the run's root span.
// Records logged without a context therefore still carry the run's trace ID.
// It returns inner unchanged if the run has not started.
func (run *Run) LogHandler(inner slog.Handler) slog.Handler {
	root := run.SpanContext()
	if !root.IsValid() {
		return inner
	}
	return &logHandler{Handler: inner, root: root}
}

// Handle implements slog.Handler.
func (handler *logHandler) Handle(ctx context.Context, record slog.Record) error {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		spanContext = handler.root
	}
	record.AddAttrs(
		slog.String("trace_id", spanContext.TraceID().String()),
		slog.String("span_id", spanContext.SpanID().String()),
	)
	return handler.Handler.Handle(ctx, record)
}

// WithAttrs implements slog.Handler.
func (handler *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{Handler: handler.Handler.WithAttrs(attrs), root: handler.root}
}

// WithGroup implements slog.Handler.
func (handler *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{Handler: handler.Handler.WithGroup(name), root: handler.root}
}
