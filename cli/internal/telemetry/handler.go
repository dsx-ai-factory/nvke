// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// logHandler adds trace_id and span_id to every record.
type logHandler struct {
	slog.Handler
	root trace.SpanContext
}

// LogHandler wraps inner so each record carries trace_id and span_id: those of
// the span in the record's context, or else those of the run's root span.
// Records logged without a context therefore still carry the run's trace ID.
// If EnableOTLP was called, each record that inner handles is also exported,
// with the same trace ID and span ID and with its attributes. Export
// failures are reported as warnings through the same handler, with the trace
// ID and span ID. It returns inner unchanged if the run has not started.
func (run *Run) LogHandler(inner slog.Handler) slog.Handler {
	if run != nil {
		run.handlerBuilt = true
	}
	root := run.SpanContext()
	if !root.IsValid() {
		return inner
	}
	local := &logHandler{Handler: inner, root: root}
	exported := run.otlpHandler()
	if exported == nil {
		return local
	}
	run.warnLogger.Store(slog.New(local))
	return &teeHandler{local: local, exported: exported, root: root}
}

// teeHandler sends each record to the local handler and to the exporter. It
// logs the levels the local handler logs, so the exporter receives the records
// that appear on stderr.
type teeHandler struct {
	local    slog.Handler
	exported slog.Handler
	root     trace.SpanContext
}

// Enabled implements slog.Handler.
func (handler *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.local.Enabled(ctx, level)
}

// Handle implements slog.Handler. It exports a copy of record and writes record
// to the local handler. An export error is not returned.
func (handler *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	// The exporter reads a record's trace ID and span ID from its context.
	if !trace.SpanContextFromContext(ctx).IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, handler.root)
	}
	_ = handler.exported.Handle(ctx, redact.Record(record))
	return handler.local.Handle(ctx, record)
}

// WithAttrs implements slog.Handler.
func (handler *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{local: handler.local.WithAttrs(attrs), exported: handler.exported.WithAttrs(redact.Attrs(attrs)), root: handler.root}
}

// WithGroup implements slog.Handler.
func (handler *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{local: handler.local.WithGroup(name), exported: handler.exported.WithGroup(name), root: handler.root}
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
