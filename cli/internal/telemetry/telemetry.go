// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry gives one nvkectl invocation one trace ID. It starts a root
// span for the command, sends the W3C traceparent header and a User-Agent on
// every request to the site controller, and adds the trace ID to nvkectl's log
// records.
//
// Spans are not exported: nothing is sent anywhere, and the spans exist so the
// trace ID is real and the controller can continue it.
package telemetry

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/dsx-ai-factory/nvke/cli"

	// defaultUserAgent is the User-Agent sent before NewRun has set one. It
	// matches the "dev" version a build without ldflags reports.
	defaultUserAgent = "nvkectl/dev"
)

// userAgent is the User-Agent value Transport sends. NewRun sets it.
var userAgent atomic.Pointer[string] //nolint:gochecknoglobals // one nvkectl invocation has one version

func currentUserAgent() string {
	if value := userAgent.Load(); value != nil {
		return *value
	}
	return defaultUserAgent
}

// Run is the trace of one nvkectl invocation. The zero value is not usable;
// build one with NewRun. The methods that read the trace ID are safe to call
// on a nil Run, which has no trace.
type Run struct {
	providerOptions []sdktrace.TracerProviderOption

	mu              sync.Mutex
	tracerProvider  *sdktrace.TracerProvider
	rootSpan        trace.Span // nil before Start and after Finish
	rootSpanContext trace.SpanContext
}

// Option configures NewRun.
type Option func(*Run)

// WithSpanProcessor registers processor with the run's tracer provider. nvkectl
// exports no spans by default; tests use this to observe them.
func WithSpanProcessor(processor sdktrace.SpanProcessor) Option {
	return func(run *Run) {
		run.providerOptions = append(run.providerOptions, sdktrace.WithSpanProcessor(processor))
	}
}

// NewRun returns a Run for an nvkectl build with the given version. Requests
// sent through Transport carry "User-Agent: nvkectl/<version>".
func NewRun(version string, options ...Option) *Run {
	userAgentValue := "nvkectl/" + version
	userAgent.Store(&userAgentValue)
	run := &Run{}
	for _, option := range options {
		option(run)
	}
	return run
}

// Start begins the run's root span, named name, and returns a context that
// carries it. It installs the run's tracer provider and the W3C propagator as
// the process-wide defaults. Calling Start again returns ctx with the existing
// root span.
func (run *Run) Start(ctx context.Context, name string) context.Context {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.rootSpan != nil {
		return trace.ContextWithSpan(ctx, run.rootSpan)
	}
	run.tracerProvider = sdktrace.NewTracerProvider(run.providerOptions...)
	otel.SetTracerProvider(run.tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, run.rootSpan = run.tracerProvider.Tracer(tracerName).Start(ctx, name)
	run.rootSpanContext = run.rootSpan.SpanContext()
	return ctx
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

type runKey struct{}

// ContextWithRun returns ctx carrying run, so that code with only a context can
// start the run's root span.
func ContextWithRun(ctx context.Context, run *Run) context.Context {
	return context.WithValue(ctx, runKey{}, run)
}

// RunFrom returns the Run ContextWithRun stored in ctx, or nil.
func RunFrom(ctx context.Context) *Run {
	run, _ := ctx.Value(runKey{}).(*Run)
	return run
}

// Transport wraps base so every request carries the traceparent of the span in
// its context and the nvkectl User-Agent. A nil base means
// http.DefaultTransport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(userAgentTransport{base: base},
		otelhttp.WithPropagators(propagation.TraceContext{}))
}
