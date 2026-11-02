// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// logHandler adds trace_id and span_id, and the run's phase, operation and
// cluster when set, to every record.
type logHandler struct {
	slog.Handler
	root trace.SpanContext
	run  *Run
}

// LogHandler wraps inner so each record carries trace_id and span_id: those of
// the span in the record's context, or else those of the run's current phase,
// or else those of the run's root span. Records logged without a context
// therefore still carry the run's trace ID. While a phase is active each record
// also carries phase, and operation and cluster once SetLogFields has set them.
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
	local := &logHandler{Handler: inner, root: root, run: run}
	exported := run.otlpHandler()
	if exported == nil {
		return local
	}
	run.warnLogger.Store(slog.New(local))
	return &teeHandler{local: local, exported: exported}
}

// teeHandler sends each record to the local handler and to the exporter. It
// logs the levels the local handler logs, so the exporter receives the records
// that appear on stderr.
type teeHandler struct {
	local    *logHandler
	exported slog.Handler
}

// Enabled implements slog.Handler.
func (handler *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.local.Enabled(ctx, level)
}

// Handle implements slog.Handler. It exports a copy of record and writes record
// to the local handler. Both get the span and fields that the local handler
// resolves, so the exported record matches the one on stderr. An export error is
// not returned.
func (handler *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	// The exporter reads a record's trace ID and span ID from its context.
	ctx, fields := handler.local.resolve(ctx, record)
	exported := record.Clone()
	exported.AddAttrs(fields...)
	_ = handler.exported.Handle(ctx, redact.Record(exported))
	return handler.local.handleResolved(ctx, record, fields)
}

// WithAttrs implements slog.Handler.
func (handler *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// A logHandler's WithAttrs returns a logHandler.
	return &teeHandler{local: handler.local.WithAttrs(attrs).(*logHandler), exported: handler.exported.WithAttrs(redact.Attrs(attrs))}
}

// WithGroup implements slog.Handler.
func (handler *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{local: handler.local.WithGroup(name).(*logHandler), exported: handler.exported.WithGroup(name)}
}

// Handle implements slog.Handler.
func (handler *logHandler) Handle(ctx context.Context, record slog.Record) error {
	ctx, fields := handler.resolve(ctx, record)
	return handler.handleResolved(ctx, record, fields)
}

// resolve returns ctx carrying the span the record belongs to, and the operation,
// cluster and phase fields to add to record. The span is the one already in ctx,
// else the current phase's, else the root span.
func (handler *logHandler) resolve(ctx context.Context, record slog.Record) (context.Context, []slog.Attr) {
	phase, operation, cluster := handler.run.logFields()
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() && phase != nil {
		spanContext = phase.span
	}
	if !spanContext.IsValid() {
		spanContext = handler.root
	}
	ctx = trace.ContextWithSpanContext(ctx, spanContext)
	var fields []slog.Attr
	// A record that already names a field keeps its own value.
	fields = appendUnset(fields, record, "operation", operation)
	fields = appendUnset(fields, record, "cluster", cluster)
	if phase != nil {
		fields = appendUnset(fields, record, "phase", phase.name)
	}
	return ctx, fields
}

// handleResolved adds trace_id and span_id from ctx, as resolve left it, then
// fields, and passes record to the inner handler. The e2e gitleaks allowlist
// depends on this order.
func (handler *logHandler) handleResolved(ctx context.Context, record slog.Record, fields []slog.Attr) error {
	spanContext := trace.SpanContextFromContext(ctx)
	record.AddAttrs(
		slog.String("trace_id", spanContext.TraceID().String()),
		slog.String("span_id", spanContext.SpanID().String()),
	)
	record.AddAttrs(fields...)
	return handler.Handler.Handle(ctx, record)
}

// WithAttrs implements slog.Handler.
func (handler *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{Handler: handler.Handler.WithAttrs(attrs), root: handler.root, run: handler.run}
}

// WithGroup implements slog.Handler.
func (handler *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{Handler: handler.Handler.WithGroup(name), root: handler.root, run: handler.run}
}

// appendUnset appends key=value to attrs unless value is empty or record has key.
func appendUnset(attrs []slog.Attr, record slog.Record, key, value string) []slog.Attr {
	if value == "" {
		return attrs
	}
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		found = attr.Key == key
		return !found
	})
	if found {
		return attrs
	}
	return append(attrs, slog.String(key, value))
}
