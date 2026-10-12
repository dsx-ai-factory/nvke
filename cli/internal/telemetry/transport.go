// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"log/slog"
	"net/http"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
)

// Transport wraps base so every request carries the traceparent of the span in
// its context and the nvkectl User-Agent. It finds the Run in the request's
// context: a request sent outside a started Run gets the default User-Agent
// and no trace. A nil base means http.DefaultTransport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &runTransport{base: base}
}

// runTransport sends each request through a tracing transport built for the
// request's Run.
type runTransport struct {
	base http.RoundTripper
	runs sync.Map // *Run -> http.RoundTripper
}

func (transport *runTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	run := RunFrom(req.Context())
	if run == nil || run.activeTracerProvider() == nil {
		return userAgentTransport{base: transport.base, userAgent: defaultUserAgent}.RoundTrip(req)
	}
	traced, ok := transport.runs.Load(run)
	if !ok {
		// The tracer provider and the User-Agent belong to the Run, so the
		// transport is built per Run.
		traced, _ = transport.runs.LoadOrStore(run, otelhttp.NewTransport(
			userAgentTransport{base: transport.base, userAgent: run.userAgent},
			otelhttp.WithTracerProvider(run.activeTracerProvider()),
			otelhttp.WithPropagators(propagation.TraceContext{}),
		))
	}
	response, err := traced.(http.RoundTripper).RoundTrip(req)
	if err == nil {
		run.checkResponseTraceID(req.Context(), response)
	}
	return response, err
}

// userAgentTransport sets the User-Agent and logs each request at debug level.
type userAgentTransport struct {
	base      http.RoundTripper
	userAgent string
}

func (transport userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", transport.userAgent)
	slog.DebugContext(req.Context(), "sending request", "method", req.Method, "path", req.URL.Path)
	return transport.base.RoundTrip(req)
}
