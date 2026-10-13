// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry gives one nvkectl invocation one trace ID. It starts a root
// span for the command, sends the W3C traceparent header and a User-Agent on
// every request to the site controller, and adds the trace ID to nvkectl's log
// records.
//
// Spans are not exported.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/dsx-ai-factory/nvke/cli"

	// defaultUserAgent is the User-Agent of a request sent outside any Run. It
	// matches the "dev" version a build without ldflags reports.
	defaultUserAgent = "nvkectl/dev"

	// traceIDHeader is the response header in which the controller reports the
	// trace ID of the request.
	traceIDHeader = "X-Trace-ID"
)

// Run is the trace of one nvkectl invocation. The zero value is not usable;
// build one with NewRun. The methods that read the trace ID are safe to call
// on a nil Run, which has no trace.
type Run struct {
	providerOptions []sdktrace.TracerProviderOption
	userAgent       string
	logger          *slog.Logger

	mismatchOnce sync.Once // the trace ID mismatch is reported once per run

	mu              sync.Mutex
	tracerProvider  *sdktrace.TracerProvider
	rootSpan        trace.Span // nil before Start and after Finish
	rootSpanContext trace.SpanContext
}

// sanitizeVersion returns version reduced to the characters of an RFC 9110
// token, or "dev" if none are left.
func sanitizeVersion(version string) string {
	// A control character in the header makes Go refuse every request.
	token := strings.Map(func(char rune) rune {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			return char
		case strings.ContainsRune("!#$%&'*+-.^_`|~", char):
			return char
		}
		return -1
	}, version)
	if token == "" {
		return "dev"
	}
	return token
}

// Option configures NewRun.
type Option func(*Run)

// WithSpanProcessor registers processor with the run's tracer provider.
func WithSpanProcessor(processor sdktrace.SpanProcessor) Option {
	return func(run *Run) {
		run.providerOptions = append(run.providerOptions, sdktrace.WithSpanProcessor(processor))
	}
}

// WithLogger sets the logger a Run uses for its warnings. The default is the
// logger returned by slog.Default when the warning is written.
func WithLogger(logger *slog.Logger) Option {
	return func(run *Run) { run.logger = logger }
}

// NewRun returns a Run for an nvkectl build with the given version. Requests
// sent through Transport carry "User-Agent: nvkectl/<version>".
func NewRun(version string, options ...Option) *Run {
	run := &Run{userAgent: "nvkectl/" + sanitizeVersion(version)}
	for _, option := range options {
		option(run)
	}
	return run
}

// Start begins the run's root span, named name, and returns a context that
// carries it and the run itself, so Transport can find them. Calling Start
// again returns ctx with the existing root span.
func (run *Run) Start(ctx context.Context, name string) context.Context {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.rootSpan != nil {
		return ContextWithRun(trace.ContextWithSpan(ctx, run.rootSpan), run)
	}
	run.tracerProvider = sdktrace.NewTracerProvider(run.providerOptions...)
	ctx, run.rootSpan = run.tracerProvider.Tracer(tracerName).Start(ctx, name)
	run.rootSpanContext = run.rootSpan.SpanContext()
	return ContextWithRun(ctx, run)
}

// SetName renames the root span, for a name that is only known after Start. It
// is a no-op before Start and after Finish.
func (run *Run) SetName(name string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.rootSpan != nil {
		run.rootSpan.SetName(name)
	}
}

// TraceID returns the run's 32-hex trace ID, or "" if Start has not been
// called. It stays valid after Finish.
func (run *Run) TraceID() string {
	spanContext := run.SpanContext()
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

// SpanContext returns the root span's context, which is invalid if Start has
// not been called. It stays valid after Finish.
func (run *Run) SpanContext() trace.SpanContext {
	if run == nil {
		return trace.SpanContext{}
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.rootSpanContext
}

// Finish ends the root span, recording err as its outcome when non-nil. It is
// a no-op if Start was not called.
func (run *Run) Finish(err error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.rootSpan == nil {
		return
	}
	if err != nil {
		run.rootSpan.RecordError(err)
		run.rootSpan.SetStatus(codes.Error, err.Error())
	}
	run.rootSpan.End()
	_ = run.tracerProvider.Shutdown(context.Background())
	run.rootSpan, run.tracerProvider = nil, nil
}

// checkResponseTraceID warns, once per run, when response carries an X-Trace-ID
// that differs from the run's trace ID. The controller reports the trace its
// logs and spans use, so a different ID means something between nvkectl and the
// controller dropped the traceparent header. A response without the header says
// nothing either way.
func (run *Run) checkResponseTraceID(ctx context.Context, response *http.Response) {
	received := response.Header.Get(traceIDHeader)
	sent := run.TraceID()
	if received == "" || sent == "" || strings.EqualFold(received, sent) {
		return
	}
	run.mismatchOnce.Do(func() {
		logger := run.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "the controller reported a different trace ID than nvkectl sent; "+
			"check that the traceparent header reaches the controller and that the controller continues incoming trace context",
			"sent", sent, "received", received)
	})
}

type runKey struct{}

// ContextWithRun returns ctx carrying run.
func ContextWithRun(ctx context.Context, run *Run) context.Context {
	return context.WithValue(ctx, runKey{}, run)
}

// RunFrom returns the Run ContextWithRun stored in ctx, or nil.
func RunFrom(ctx context.Context) *Run {
	run, _ := ctx.Value(runKey{}).(*Run)
	return run
}

// activeTracerProvider returns the run's tracer provider, or nil before Start
// and after Finish.
func (run *Run) activeTracerProvider() *sdktrace.TracerProvider {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.tracerProvider
}
