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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

var traceparentRE = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`)

func newTestRun(t *testing.T) (*Run, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	return NewRun("v1.2.3", WithSpanProcessor(rec)), rec
}

func TestTransportInjectsTraceparentAndUserAgent(t *testing.T) {
	t.Parallel()
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
	resp, err := (&http.Client{Transport: Transport(srv.Client().Transport)}).Do(req)
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
	t.Parallel()
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
	t.Parallel()
	run := NewRun("dev")
	assert.Empty(t, run.TraceID())
	run.Finish(nil) // must not panic
	assert.Empty(t, (*Run)(nil).TraceID(), "a nil Run has no trace")
	inner := slog.NewTextHandler(&bytes.Buffer{}, nil)
	assert.Same(t, inner, run.LogHandler(inner))
}

func TestLogHandlerAddsTraceFields(t *testing.T) {
	t.Parallel()
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

func TestLogHandlerUsesTheChildSpanInContext(t *testing.T) {
	t.Parallel()
	run, _ := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl get clusters")
	childCtx, child := trace.SpanFromContext(ctx).TracerProvider().Tracer("test").Start(ctx, "child")
	defer child.End()

	var buf bytes.Buffer
	slog.New(run.LogHandler(slog.NewJSONHandler(&buf, nil))).InfoContext(childCtx, "inside the child")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, run.TraceID(), rec["trace_id"], "the child shares the run's trace")
	assert.Equal(t, child.SpanContext().SpanID().String(), rec["span_id"], "the child's own span ID, not the root's")
	assert.NotEqual(t, run.SpanContext().SpanID().String(), rec["span_id"])
}

func TestTransportOutsideARunSendsDefaultUserAgentAndNoTrace(t *testing.T) {
	t.Parallel()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()

	resp, err := (&http.Client{Transport: Transport(srv.Client().Transport)}).Get(srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, "nvkectl/dev", got.Get("User-Agent"))
	assert.Empty(t, got.Get("Traceparent"))
}

// The tests that send requests pass their own server's transport, so this is
// the only cover for the documented nil-base fallback.
func TestTransportDefaultsToTheSharedDefaultTransport(t *testing.T) {
	t.Parallel()
	transport, ok := Transport(nil).(*runTransport)
	require.True(t, ok)
	assert.Same(t, http.DefaultTransport, transport.base)
}

func TestRunsKeepTheirOwnUserAgent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen[r.URL.Path] = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	client := &http.Client{Transport: Transport(srv.Client().Transport)}

	for _, version := range []string{"v1", "v2"} {
		run := NewRun(version)
		ctx := run.Start(context.Background(), "nvkectl get clusters")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+version, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		run.Finish(nil)
	}

	assert.Equal(t, map[string]string{"/v1": "nvkectl/v1", "/v2": "nvkectl/v2"}, seen,
		"two runs in one process do not share a User-Agent")
}

func TestSetNameRenamesTheRootSpan(t *testing.T) {
	t.Parallel()
	run, rec := newTestRun(t)
	run.Start(context.Background(), "nvkectl")
	run.SetName("nvkectl get clusters")
	run.Finish(nil)
	run.SetName("ignored after Finish")

	spans := rec.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "nvkectl get clusters", spans[0].Name())
}

func TestSanitizeVersion(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"v1.2.3":                  "v1.2.3",
		"v1.2.3-5-gabc1234-dirty": "v1.2.3-5-gabc1234-dirty",
		"v1.2.3+dirty":            "v1.2.3+dirty",
		"v1.2.3\n":                "v1.2.3",
		"v1\r\n2":                 "v12",
		"1.0 beta":                "1.0beta",
		"feat/x":                  "featx",
		"(devel)":                 "devel",
		"\x00\t":                  "dev",
		"":                        "dev",
	}
	for input, want := range tests {
		assert.Equal(t, want, sanitizeVersion(input), "%q", input)
	}
}

// Go refuses to send a request whose header holds a control character, so a
// version stamped with one must not reach the User-Agent.
func TestMalformedVersionStillSendsRequests(t *testing.T) {
	t.Parallel()
	var userAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	run := NewRun("v1\r\n2")
	ctx := run.Start(context.Background(), "nvkectl get clusters")
	defer run.Finish(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: Transport(srv.Client().Transport)}).Do(req)
	require.NoError(t, err, "the request must not be rejected over its User-Agent")
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, "nvkectl/v12", userAgent)
}

func TestWarnsWhenTheControllerReportsADifferentTraceID(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		header   func(sent string) string // the X-Trace-ID the controller returns; "" sends none
		wantWarn bool
	}{
		"a different ID":     {header: func(string) string { return "0af7651916cd43dd8448eb211c80319c" }, wantWarn: true},
		"the same ID":        {header: func(sent string) string { return sent }},
		"the same ID, cased": {header: func(sent string) string { return strings.ToUpper(sent) }},
		"no header":          {header: func(string) string { return "" }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			run := NewRun("v1", WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
			ctx := run.Start(context.Background(), "nvkectl get clusters")
			defer run.Finish(nil)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if id := tc.header(run.TraceID()); id != "" {
					w.Header().Set("X-Trace-ID", id)
				}
			}))
			defer srv.Close()

			// Two requests: the warning is for the run, not for each request.
			for range 2 {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
				require.NoError(t, err)
				resp, err := (&http.Client{Transport: Transport(srv.Client().Transport)}).Do(req)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}

			if !tc.wantWarn {
				assert.Empty(t, logs.String())
				return
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			require.Len(t, lines, 1, "warned once")
			var rec map[string]any
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
			assert.Equal(t, "WARN", rec["level"])
			assert.Equal(t, run.TraceID(), rec["sent"])
			assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", rec["received"])
		})
	}
}

// reentrantProcessor is a span processor that reads the run's trace ID from the
// callbacks the SDK makes while the run finishes.
type reentrantProcessor struct {
	run *Run

	endID      string
	shutdownID string
}

func (*reentrantProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (p *reentrantProcessor) OnEnd(sdktrace.ReadOnlySpan)                   { p.endID = p.run.TraceID() }

func (p *reentrantProcessor) Shutdown(context.Context) error {
	p.shutdownID = p.run.TraceID()
	return nil
}
func (*reentrantProcessor) ForceFlush(context.Context) error { return nil }

// Finish does not hold the run's lock while the SDK calls span processors.
func TestFinishAllowsProcessorCallbacksToReadTheTraceID(t *testing.T) {
	t.Parallel()
	processor := &reentrantProcessor{}
	run := NewRun("test", WithSpanProcessor(processor))
	processor.run = run
	run.Start(context.Background(), "probe")

	done := make(chan struct{})
	go func() {
		defer close(done)
		run.Finish(errors.New("failed"))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Finish deadlocked: a span processor callback could not read the trace ID")
	}

	assert.Len(t, run.TraceID(), 32)
	assert.Equal(t, run.TraceID(), processor.endID, "OnEnd saw the trace ID")
	assert.Equal(t, run.TraceID(), processor.shutdownID, "Shutdown saw the trace ID")
}

type taggedError struct{ msg string }

func (e *taggedError) Error() string { return e.msg }

// Finish records the error as it is, which keeps its type, and the exporter
// masks its text.
func TestFinishKeepsTheErrorsTypeAndTheExporterMasksItsText(t *testing.T) {
	const token = "finish-token-6e7f8a9b"
	RegisterHeaderSecrets(map[string]string{"authorization": "Bearer " + token})
	for name, tt := range map[string]struct {
		err      error
		wantType string
		wantText string
	}{
		"nothing to mask": {&taggedError{"plain failure"}, "*telemetry.taggedError", "plain failure"},
		"masked":          {&taggedError{"failure with " + token}, "*telemetry.taggedError", "failure with <redacted>"},
	} {
		t.Run(name, func(t *testing.T) {
			collector := newFakeCollector(t)
			run := NewRun("dev")
			require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
			run.Start(context.Background(), "nvkectl get clusters")

			original := tt.err.Error()
			run.Finish(tt.err)

			spans := collector.receivedSpans()
			require.NotEmpty(t, spans)
			var types, messages []string
			for _, event := range spans[0].GetEvents() {
				for _, kv := range event.GetAttributes() {
					switch kv.GetKey() {
					case "exception.type":
						types = append(types, kv.GetValue().GetStringValue())
					case "exception.message":
						messages = append(messages, kv.GetValue().GetStringValue())
					}
				}
			}
			assert.Equal(t, []string{tt.wantType}, types)
			assert.Equal(t, []string{tt.wantText}, messages)
			assert.Equal(t, tt.wantText, spans[0].GetStatus().GetMessage())
			assert.Equal(t, original, tt.err.Error(), "the error itself is untouched")
		})
	}
}
