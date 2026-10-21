// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// fakeCollector is an in-process OTLP/gRPC logs and traces endpoint.
type fakeCollector struct {
	collogspb.UnimplementedLogsServiceServer

	addr string

	mu           sync.Mutex
	records      []*logspb.LogRecord
	spans        []*tracepb.Span
	resource     map[string]string
	spanResource map[string]string
	metadata     metadata.MD
	spanMetadata metadata.MD
}

func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	collector := &fakeCollector{addr: lis.Addr().String()}
	server := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(server, collector)
	coltracepb.RegisterTraceServiceServer(server, &traceCollector{collector: collector})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return collector
}

// Export implements collogspb.LogsServiceServer.
func (c *fakeCollector) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metadata, _ = metadata.FromIncomingContext(ctx)
	for _, resourceLogs := range req.GetResourceLogs() {
		c.resource = map[string]string{}
		for _, kv := range resourceLogs.GetResource().GetAttributes() {
			c.resource[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			c.records = append(c.records, scopeLogs.GetLogRecords()...)
		}
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (c *fakeCollector) received() []*logspb.LogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*logspb.LogRecord(nil), c.records...)
}

// exportSpans records the spans of one trace export request.
func (c *fakeCollector) exportSpans(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spanMetadata, _ = metadata.FromIncomingContext(ctx)
	for _, resourceSpans := range req.GetResourceSpans() {
		c.spanResource = map[string]string{}
		for _, kv := range resourceSpans.GetResource().GetAttributes() {
			c.spanResource[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			c.spans = append(c.spans, scopeSpans.GetSpans()...)
		}
	}
}

func (c *fakeCollector) receivedSpans() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...)
}

// traceCollector adapts fakeCollector to the trace service, whose Export method
// would otherwise collide with the logs service's.
type traceCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	collector *fakeCollector
}

// Export implements coltracepb.TraceServiceServer.
func (t *traceCollector) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	t.collector.exportSpans(ctx, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// lockedBuffer is a bytes.Buffer that an export goroutine and the test can use together.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func attributes(record *logspb.LogRecord) map[string]string {
	out := map[string]string{}
	for _, kv := range record.GetAttributes() {
		out[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return out
}

func TestOTLPExportsRecordsWithTraceFields(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)
	stderr := &lockedBuffer{}

	run := NewRun("v1.2.3")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	logger := slog.New(run.LogHandler(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	logger.Info("provisioning", "phase", "kind")
	logger.Debug("not shown on stderr, so not exported")
	logger.InfoContext(ctx, "with context", "node", "n1")
	run.Finish(nil)

	records := collector.received()
	require.Len(t, records, 2, "the debug record is below the stderr level")
	wantTraceID, wantSpanID := run.SpanContext().TraceID(), run.SpanContext().SpanID()
	for _, record := range records {
		assert.Equal(t, hex.EncodeToString(wantTraceID[:]), hex.EncodeToString(record.GetTraceId()))
		assert.Equal(t, hex.EncodeToString(wantSpanID[:]), hex.EncodeToString(record.GetSpanId()))
	}
	assert.Equal(t, "provisioning", records[0].GetBody().GetStringValue())
	assert.Equal(t, "kind", attributes(records[0])["phase"])
	assert.Equal(t, "n1", attributes(records[1])["node"])
	assert.Equal(t, "nvkectl", collector.resource["service.name"])
	assert.Equal(t, "v1.2.3", collector.resource["service.version"])

	// stderr gets the text handler's line plus the trace fields.
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "msg=provisioning phase=kind trace_id="+run.TraceID())
}

func TestOTLPSendsHeadersAndNeverLogsThem(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{
		Endpoint: collector.addr,
		Insecure: true,
		Headers:  map[string]string{"authorization": "Bearer s3cret"},
	}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
	run.Finish(nil)

	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Equal(t, []string{"Bearer s3cret"}, collector.metadata.Get("authorization"))
	assert.Equal(t, []string{"Bearer s3cret"}, collector.spanMetadata.Get("authorization"), "the spans carry them too")
	assert.NotContains(t, stderr.String(), "s3cret")
}

func TestOTLPUnreachableEndpointWarnsOnceAndFinishIsBounded(t *testing.T) {
	t.Parallel()
	// A port nothing listens on: bind, note the address, close.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl create cluster")
	logger := slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil)))
	for range 20 {
		logger.Info("record")
	}

	started := time.Now()
	run.Finish(nil)
	assert.Less(t, time.Since(started), flushTimeout+time.Second, "Finish waits at most the flush bound")

	assert.Equal(t, 1, strings.Count(stderr.String(), "could not export telemetry"), "stderr:\n%s", stderr.String())
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, "could not export telemetry") {
			assert.Contains(t, line, "trace_id="+run.TraceID(), "the warning carries the run's trace ID")
		}
	}
	assert.Equal(t, 20, strings.Count(stderr.String(), "msg=record"), "local output is unaffected")
}

func TestOTLPExportsSpansWithRunTraceID(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)

	run := NewRun("v1.2.3")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl create cluster")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: Transport(srv.Client().Transport)}).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	run.Finish(nil)

	spans := collector.receivedSpans()
	require.Len(t, spans, 2, "the root span and the HTTP client span")
	var root, client *tracepb.Span
	for _, span := range spans {
		if span.GetName() == "nvkectl create cluster" {
			root = span
		} else {
			client = span
		}
	}
	require.NotNil(t, root)
	require.NotNil(t, client)
	for _, span := range spans {
		assert.Equal(t, run.TraceID(), hex.EncodeToString(span.GetTraceId()))
	}
	assert.Equal(t, hex.EncodeToString(root.GetSpanId()), hex.EncodeToString(client.GetParentSpanId()))
	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Equal(t, "nvkectl", collector.spanResource["service.name"])
	assert.Equal(t, "v1.2.3", collector.spanResource["service.version"])
}

// The span exporter is added whether EnableOTLP runs before the run starts or
// after it, which is the order PersistentPreRunE uses.
func TestOTLPExportsSpansWhenEnabled(t *testing.T) {
	for _, enableBeforeStart := range []bool{true, false} {
		t.Run(fmt.Sprintf("before start=%v", enableBeforeStart), func(t *testing.T) {
			t.Parallel()
			collector := newFakeCollector(t)

			run := NewRun("dev")
			enable := func() { require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true})) }
			if enableBeforeStart {
				enable()
			}
			run.Start(context.Background(), "nvkectl get clusters")
			if !enableBeforeStart {
				enable()
			}
			run.Finish(nil)

			spans := collector.receivedSpans()
			require.Len(t, spans, 1, "the root span is exported either way")
			assert.Equal(t, run.TraceID(), hex.EncodeToString(spans[0].GetTraceId()))
		})
	}
}

func TestOTLPUnreachableEndpointWarnsOnceForBothSignals(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl create cluster")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("record")

	started := time.Now()
	run.Finish(nil)
	assert.Less(t, time.Since(started), flushTimeout+500*time.Millisecond, "both signals share one flush bound")

	assert.Equal(t, 1, strings.Count(stderr.String(), "could not export telemetry"), "stderr:\n%s", stderr.String())
}

func TestRunOTLPReportsTheEnabledConfig(t *testing.T) {
	t.Parallel()
	run := NewRun("dev")
	cfg, ok := run.OTLP()
	assert.False(t, ok)
	assert.Equal(t, OTLPConfig{}, cfg)

	want := OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{"x-team": "nke"}}
	require.NoError(t, run.EnableOTLP(want))
	cfg, ok = run.OTLP()
	assert.True(t, ok)
	assert.Equal(t, want, cfg)
}

func TestWithoutOTLPTheHandlerIsUnchanged(t *testing.T) {
	t.Parallel()
	run := NewRun("dev")
	run.Start(context.Background(), "nvkectl get clusters")
	handler := run.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	assert.IsType(t, &logHandler{}, handler, "no tee, no exporter")
	run.Finish(nil) // flushing with nothing enabled is a no-op
}

func TestEnableOTLPRejectsBadUse(t *testing.T) {
	t.Parallel()
	run := NewRun("dev")
	assert.Error(t, run.EnableOTLP(OTLPConfig{}), "an empty endpoint")

	run.Start(context.Background(), "nvkectl get clusters")
	run.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	assert.Error(t, run.EnableOTLP(OTLPConfig{Endpoint: "localhost:4317"}), "after the handler was built")

	early := NewRun("dev")
	early.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)) // before Start, so it returns inner
	assert.Error(t, early.EnableOTLP(OTLPConfig{Endpoint: "localhost:4317"}), "after LogHandler on a run that has not started")

	twice := NewRun("dev")
	require.NoError(t, twice.EnableOTLP(OTLPConfig{Endpoint: "localhost:4317"}))
	assert.Error(t, twice.EnableOTLP(OTLPConfig{Endpoint: "localhost:4318"}), "a second call")
}

// The signal-specific variables the OTLP exporter library would read on its own.
const (
	logsEndpointVar = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	logsHeadersVar  = "OTEL_EXPORTER_OTLP_LOGS_HEADERS"
	logsInsecureVar = "OTEL_EXPORTER_OTLP_LOGS_INSECURE"
)

// These tests set the exporter library's own signal-specific variables and check
// that none changes the destination, the headers, the compression or TLS. They
// set variables, so neither is parallel.
func TestOTLPExporterIgnoresSignalSpecificVariables(t *testing.T) {
	collector := newFakeCollector(t)
	t.Setenv(logsEndpointVar, "127.0.0.1:1")
	t.Setenv(logsHeadersVar, "x-leaked=yes")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "gzip")

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
	run.Finish(nil)

	require.Len(t, collector.received(), 1, "the records went to the configured endpoint")
	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Empty(t, collector.metadata.Get("x-leaked"), "no header from OTEL_EXPORTER_OTLP_LOGS_HEADERS")
	assert.Empty(t, collector.metadata.Get("grpc-encoding"), "no compression from OTEL_EXPORTER_OTLP_LOGS_COMPRESSION")
}

// A bad certificate variable, which the exporter library reads and nvkectl does
// not use, must not spend the run's one warning: a real export failure after it
// still gives exactly one warning, with the trace ID. It sets the global log
// output and a variable, so it is not parallel.
func TestOTLPBadCertificateVariableDoesNotSpendTheExportWarning(t *testing.T) {
	var printed bytes.Buffer
	log.SetOutput(&printed)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "/nonexistent/ca.pem")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := lis.Addr().String() // nothing listens here, so every export fails
	require.NoError(t, lis.Close())
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: closed, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
	run.Finish(nil)

	var warnings []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, "could not export telemetry") {
			warnings = append(warnings, line)
		}
	}
	require.Len(t, warnings, 1, "stderr:\n%s", stderr.String())
	assert.Contains(t, warnings[0], "trace_id="+run.TraceID())
	assert.NotContains(t, stderr.String(), "failed to load TLS", "the library's own line is not turned into the warning")
	assert.Contains(t, printed.String(), "failed to load TLS", "the library prints the bad variable itself")
}

// EnableOTLP leaves the OpenTelemetry error handler to its owner: an error
// another component reports reaches that handler and does not use up the run's
// export warning. It installs a global handler, so it is not parallel.
func TestEnableOTLPLeavesTheGlobalErrorHandlerAlone(t *testing.T) {
	var mu sync.Mutex
	var got []error
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, err)
	}))
	// The library offers no way to put its default handler back, so log as it does.
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Print(err) })) })
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: "127.0.0.1:1", Insecure: true}))
	otel.Handle(errors.New("another component's error"))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")

	mu.Lock()
	assert.Len(t, got, 1, "the handler its owner installed received the error")
	mu.Unlock()
	assert.NotContains(t, stderr.String(), "could not export telemetry")
	run.Finish(nil)
}

// A malformed variable the exporter reads on its own must not reach stderr with
// its value. It sets the global log output, so it is not parallel.
func TestOTLPDoesNotPrintTheValueOfAnotherEndpointVariable(t *testing.T) {
	var printed bytes.Buffer
	log.SetOutput(&printed)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	t.Setenv(logsEndpointVar, "https://user:s3cret@collector/%zz")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE", "/nonexistent/ca.pem")

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: "127.0.0.1:1", Insecure: true}))

	assert.NotContains(t, printed.String(), "s3cret")
	assert.NotContains(t, printed.String(), logsEndpointVar, "the exporter did not read the variable")
}

func TestOTLPSecureConfigIgnoresTheInsecureLogsVariable(t *testing.T) {
	collector := newFakeCollector(t) // serves plaintext, so a TLS client cannot reach it
	t.Setenv(logsInsecureVar, "true")
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
	run.Finish(nil)

	assert.Empty(t, collector.received(), "the connection used TLS, as configured")
	assert.Contains(t, stderr.String(), "could not export telemetry")
}

func TestOTLPConfigFromEnv(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		want   OTLPConfig
		wantOK bool
	}{
		{name: "nothing set"},
		{
			name:   "host and port",
			env:    map[string]string{envEndpoint: "collector:4317"},
			want:   OTLPConfig{Endpoint: "collector:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "IPv6 host and port",
			env:    map[string]string{envEndpoint: "[::1]:4317"},
			want:   OTLPConfig{Endpoint: "[::1]:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "trailing slash is not a path",
			env:    map[string]string{envEndpoint: "https://collector:4317/"},
			want:   OTLPConfig{Endpoint: "collector:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "http URL is insecure",
			env:    map[string]string{envEndpoint: "http://collector:4317"},
			want:   OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "https URL is secure",
			env:    map[string]string{envEndpoint: "https://collector:4317"},
			want:   OTLPConfig{Endpoint: "collector:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "https URL stays secure when the insecure variable is true",
			env:    map[string]string{envEndpoint: "https://collector:4317", envInsecure: "true"},
			want:   OTLPConfig{Endpoint: "collector:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "http URL is insecure when the insecure variable is false",
			env:    map[string]string{envEndpoint: "http://collector:4317", envInsecure: "false"},
			want:   OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "insecure variable",
			env:    map[string]string{envEndpoint: "collector:4317", envInsecure: "true"},
			want:   OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "signal-specific variables are ignored",
			env:    map[string]string{envEndpoint: "general:4317", logsEndpointVar: "logs:4317", logsHeadersVar: "x-logs=1", logsInsecureVar: "true"},
			want:   OTLPConfig{Endpoint: "general:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name: "only a signal-specific endpoint is no endpoint",
			env:  map[string]string{logsEndpointVar: "logs:4317"},
		},
		{
			name: "headers",
			env: map[string]string{
				envEndpoint: "collector:4317",
				envHeaders:  "authorization=Bearer%20abc, x-team=nke,malformed",
			},
			want: OTLPConfig{
				Endpoint: "collector:4317",
				Headers:  map[string]string{"authorization": "Bearer abc", "x-team": "nke"},
			},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{envEndpoint, envInsecure, envHeaders, logsEndpointVar, logsHeadersVar, logsInsecureVar} {
				t.Setenv(name, tt.env[name])
			}
			got, ok, err := OTLPConfigFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestOTLPConfigFromEnvRejectsABadEndpointWithoutPrintingIt(t *testing.T) {
	tests := map[string]string{
		"URL without a host":            "http://",
		"URL with credentials, no host": "https://user:s3cret@",
		"URL with credentials":          "https://user:s3cret@collector:4317",
		"host:port with credentials":    "user:s3cret@collector:4317",
		"unparsable":                    "collector:4317 s3cret",
		"OTLP/HTTP URL with a path":     "http://collector:4318/v1/logs",
		"unsupported scheme":            "ftp://collector:4317",
		"grpc scheme":                   "grpc://collector:4317",
		"query":                         "collector:4317?token=s3cret",
	}
	for name, endpoint := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envEndpoint, endpoint)
			_, ok, err := OTLPConfigFromEnv()
			require.Error(t, err)
			assert.False(t, ok)
			assert.Contains(t, err.Error(), envEndpoint)
			assert.NotContains(t, err.Error(), "s3cret")
		})
	}
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Endpoint
		wantErr bool
	}{
		{name: "bare host and port", raw: "host:4317", want: Endpoint{Host: "host:4317"}},
		{name: "http scheme is insecure", raw: "http://host:4317", want: Endpoint{Host: "host:4317", Insecure: true, hasScheme: true}},
		{name: "https scheme is secure", raw: "https://host", want: Endpoint{Host: "host", HTTPS: true, hasScheme: true}},
		{name: "trailing slash is not a path", raw: "https://host:4317/", want: Endpoint{Host: "host:4317", HTTPS: true, hasScheme: true}},
		{name: "scheme without a host", raw: "http://", wantErr: true},
		{name: "port without a host", raw: ":4317", wantErr: true},
		{name: "port without a host in a URL", raw: "http://:4317", wantErr: true},
		{name: "host with an empty port", raw: "host:", wantErr: true},
		{name: "host with an empty port in a URL", raw: "https://host:", wantErr: true},
		{name: "IPv6 with an empty port", raw: "[::1]:", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "bare IPv6 address", raw: "::1", wantErr: true},
		{name: "IPv6 address with a port but no brackets", raw: "fe80::1:4317", wantErr: true},
		{name: "IPv6 address without brackets in a URL", raw: "http://fe80::1:4317", wantErr: true},
		{name: "bracketed IPv6 with a port", raw: "[::1]:4317", want: Endpoint{Host: "[::1]:4317"}},
		{name: "bracketed IPv6 without a port", raw: "[::1]", want: Endpoint{Host: "[::1]"}},
		{name: "IPv6 in a URL", raw: "http://[::1]:4317", want: Endpoint{Host: "[::1]:4317", Insecure: true, hasScheme: true}},
		{name: "credentials", raw: "https://user:s3cret@host:4317", wantErr: true},
		{name: "OTLP/HTTP URL with a path", raw: "http://host:4318/v1/traces", wantErr: true},
		{name: "query", raw: "host:4317?token=s3cret", wantErr: true},
		{name: "fragment", raw: "https://host:4317#s3cret", wantErr: true},
		{name: "unsupported scheme", raw: "grpc://host:4317", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseEndpoint(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "s3cret", "the error does not contain the endpoint")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRejectLoopback(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "localhost with port", endpoint: "localhost:4317", wantErr: true},
		{name: "localhost without port", endpoint: "localhost", wantErr: true},
		{name: "localhost in capitals", endpoint: "LOCALHOST:4317", wantErr: true},
		{name: "localhost in capitals without port", endpoint: "LOCALHOST", wantErr: true},
		{name: "127.0.0.1", endpoint: "127.0.0.1:4317", wantErr: true},
		{name: "another address in 127.0.0.0/8", endpoint: "127.1.2.3:4317", wantErr: true},
		{name: "bracketed IPv6 loopback", endpoint: "[::1]", wantErr: true},
		{name: "bracketed IPv6 loopback with port", endpoint: "[::1]:4317", wantErr: true},
		{name: "unspecified IPv4", endpoint: "0.0.0.0:4317", wantErr: true},
		{name: "unspecified IPv6", endpoint: "[::]:4317", wantErr: true},
		{name: "zoned IPv6 loopback", endpoint: "[::1%25lo0]:4317", wantErr: true},
		{name: "IPv4-mapped loopback", endpoint: "[::ffff:127.0.0.1]:4317", wantErr: true},
		{name: "IPv4-mapped private address", endpoint: "[::ffff:10.0.0.5]:4317"},
		{name: "localhost with a trailing dot", endpoint: "localhost.:4317", wantErr: true},
		{name: "name under .localhost", endpoint: "foo.localhost:4317", wantErr: true},
		{name: "name under .localhost in capitals", endpoint: "FOO.LOCALHOST:4317", wantErr: true},
		{name: "host that merely starts with localhost", endpoint: "localhost.example.com:4317"},
		{name: "private address", endpoint: "10.0.0.5:4317"},
		{name: "DNS name", endpoint: "collector.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RejectLoopback(tt.endpoint)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.endpoint)
			assert.Contains(t, err.Error(), "temporary controller")
			assert.Contains(t, err.Error(), "cannot reach")
		})
	}
}

// clearOTLPEnv empties every OTEL_EXPORTER_OTLP_* variable the exporters read,
// so a test does not depend on the shell it runs in.
func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envEndpoint, logsEndpointVar, envInsecure, envHeaders, logsHeadersVar,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_INSECURE", "OTEL_EXPORTER_OTLP_LOGS_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	} {
		t.Setenv(name, "")
	}
}

// Only the configuration decides the transport: an http scheme or an insecure
// variable in the environment does not switch TLS off, so a secure configuration
// never sends a token in clear, and an insecure one wins over an https endpoint
// in the environment.
func TestEnableOTLPTransportFollowsTheConfigurationAlone(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		insecure  bool
		wantSpans int
	}{
		{name: "http endpoint", env: map[string]string{envEndpoint: "http://other.example:4317"}},
		{name: "http traces endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://other.example:4317"}},
		{name: "http logs endpoint", env: map[string]string{logsEndpointVar: "http://other.example:4317"}},
		{name: "insecure variable", env: map[string]string{envInsecure: "true"}},
		{name: "insecure traces variable", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_INSECURE": "true"}},
		{name: "insecure logs variable", env: map[string]string{"OTEL_EXPORTER_OTLP_LOGS_INSECURE": "true"}},
		{name: "insecure configuration wins over an https endpoint", env: map[string]string{envEndpoint: "https://other.example:4317"}, insecure: true, wantSpans: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOTLPEnv(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			collector := newFakeCollector(t)
			stderr := &lockedBuffer{}

			run := NewRun("dev")
			require.NoError(t, run.EnableOTLP(OTLPConfig{
				Endpoint: collector.addr,
				Insecure: tt.insecure,
				Headers:  map[string]string{"authorization": "Bearer s3cret"},
			}))
			run.Start(context.Background(), "nvkectl get clusters")
			slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
			run.Finish(nil)

			assert.Len(t, collector.receivedSpans(), tt.wantSpans)
			if tt.insecure {
				return
			}
			assert.Empty(t, collector.received(), "no record reaches a plaintext collector")
			collector.mu.Lock()
			defer collector.mu.Unlock()
			assert.Empty(t, collector.spanMetadata.Get("authorization"), "the token is not sent in clear")
			assert.Empty(t, collector.metadata.Get("authorization"), "the token is not sent in clear")
		})
	}
}

func TestOTLPGeneralEndpointExportsBoth(t *testing.T) {
	clearOTLPEnv(t)
	collector := newFakeCollector(t)
	t.Setenv(envEndpoint, "http://"+collector.addr)

	cfg, ok, err := OTLPConfigFromEnv()
	require.NoError(t, err)
	require.True(t, ok)
	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(cfg))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
	run.Finish(nil)

	assert.Len(t, collector.received(), 1)
	assert.Len(t, collector.receivedSpans(), 1)
}

// An endpoint the exporters would reject as a URL, such as a scoped IPv6 address
// or one that is not a host:port, is an error, and never makes them fall back to
// an endpoint in the environment.
func TestEnableOTLPNeverFallsBackToTheEnvironmentsEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name     string
		endpoint string
	}{
		{name: "a scoped IPv6 address", endpoint: "[fe80::1%eth0]:4317"},
		{name: "a host with a space", endpoint: "bad host:4317"},
		{name: "a port with no host", endpoint: ":4317"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearOTLPEnv(t)
			environment := newFakeCollector(t)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://"+environment.addr)
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://"+environment.addr)

			run := NewRun("dev")
			err := run.EnableOTLP(OTLPConfig{Endpoint: tt.endpoint, Insecure: true, Headers: map[string]string{"authorization": "Bearer s3cret"}})
			require.Error(t, err)
			run.Start(context.Background(), "nvkectl get clusters")
			slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
			run.Finish(nil)

			assert.Empty(t, environment.receivedSpans(), "no span reaches the environment's endpoint")
			assert.Empty(t, environment.received(), "no record reaches the environment's endpoint")
		})
	}
}

// A malformed OTEL_EXPORTER_OTLP_HEADERS entry, which can be a token, is not
// printed: not by OpenTelemetry's internal logger, which EnableOTLP discards, nor
// to the log package or the process's stderr. It sets global loggers, so it is
// not parallel.
func TestEnableOTLPNeverPrintsAMalformedHeaderEntry(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv(envHeaders, "Bearer s3cret-token")
	collector := newFakeCollector(t)

	var internal lockedBuffer
	otel.SetLogger(funcr.New(func(prefix, args string) { _, _ = internal.Write([]byte(prefix + " " + args + "\n")) }, funcr.Options{}))
	// Every EnableOTLP leaves the internal logger discarded, so the cleanup does too.
	t.Cleanup(func() { otel.SetLogger(logr.Discard()) })
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	realStderr := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = realStderr })

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
	run.Finish(nil)

	os.Stderr = realStderr
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.NotContains(t, internal.String(), "s3cret-token", "the internal logger is discarded")
	assert.NotContains(t, logged.String(), "s3cret-token")
	assert.NotContains(t, string(printed), "s3cret-token")
}

// sdkParseHeaders reads OTEL_EXPORTER_OTLP_HEADERS as the OpenTelemetry Go SDK
// does (envconfig.stringToHeader in otlptracegrpc), which is how the controller
// reads what nvkectl gives it: it splits each pair at the first "=" and drops a
// pair whose name is not an HTTP token or whose value does not percent-decode.
func sdkParseHeaders(raw string) map[string]string {
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		name, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if !isHTTPToken(name) {
			continue
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			continue
		}
		headers[name] = strings.TrimSpace(decoded)
	}
	return headers
}

func isTokenChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

func isHTTPToken(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !isTokenChar(c) {
			return false
		}
	}
	return true
}

func TestFormatHeadersRoundTrips(t *testing.T) {
	for _, raw := range []string{
		"",
		"authorization=Bearer abc123",
		"authorization=Bearer%20abc123,x-tenant=acme",
		"a=b%2Cc%3Dd,k=100%25,z=sp ace",
		"k=ünï,j=~-._",
		"authorization=Bearer a%2Cb%3Dc%25d,x-scope-orgid=nvke",
		"k=$&+:=@,j=a+b;c/d?e",
	} {
		t.Run(raw, func(t *testing.T) {
			parsed := parseHeaders(raw)
			assert.Equal(t, parsed, parseHeaders(FormatHeaders(parsed)), "nvkectl's own parser")
			assert.Equal(t, parsed, sdkParseHeaders(FormatHeaders(parsed)), "the SDK's parser, as the controller reads it")
		})
	}
	assert.Equal(t, "a=1%2C2=3%25%20x,b=y", FormatHeaders(map[string]string{"b": "y", "a": "1,2=3% x"}), "sorted, encoded")

	// JSON output, such as `kubectl cluster-info dump`, writes "&" as \u0026, so
	// the serialised string must not hold one for redact.Tree to find it there.
	formatted := FormatHeaders(map[string]string{"authorization": "Bearer s3cr&t"})
	encoded, err := json.Marshal(formatted)
	require.NoError(t, err)
	assert.Equal(t, `"`+formatted+`"`, string(encoded))
}

// headerSecrets registers every header pair; a value on its own, and the token of
// a `scheme token` value, only for a header with a credential name.
func TestHeaderSecrets(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    []string // registered
		wantNot []string // not registered
	}{
		{
			name: "a team name", headers: map[string]string{"X-Scope-OrgID": "nvke"},
			want: []string{"X-Scope-OrgID=nvke"}, wantNot: []string{"nvke"},
		},
		{
			name: "a boolean", headers: map[string]string{"x-flag": "true"},
			want: []string{"x-flag=true"}, wantNot: []string{"true"},
		},
		{
			name: "a short value of a credential header", headers: map[string]string{"x-api-key": "k3y-9z"},
			want: []string{"x-api-key=k3y-9z"}, wantNot: []string{"k3y-9z"},
		},
		{
			name: "a boolean under a credential name", headers: map[string]string{"x-auth-enabled": "true"},
			want: []string{"x-auth-enabled=true"}, wantNot: []string{"true"},
		},
		{
			name: "a one-letter token after a scheme", headers: map[string]string{"authorization": "Bearer x"},
			want: []string{"authorization=Bearer x"}, wantNot: []string{"x"},
		},
		{
			name: "vendor headers that carry a key", headers: map[string]string{"x-honeycomb-team": "hcaik_0123456789abcdef", "uptrace-dsn": "https://token@uptrace.example/1"},
			want: []string{"hcaik_0123456789abcdef", "https://token@uptrace.example/1"},
		},
		{
			name: "an ampersand is encoded", headers: map[string]string{"authorization": "Bearer s3cr&t-value"},
			want: []string{"authorization=Bearer%20s3cr%26t-value", "Bearer%20s3cr%26t-value", "s3cr&t-value"},
		},
		{
			name:    "a scheme and a token",
			headers: map[string]string{"authorization": "Bearer example-token"},
			want:    []string{"authorization=Bearer example-token", "authorization=Bearer%20example-token", "Bearer example-token", "Bearer%20example-token", "example-token"},
			wantNot: []string{"Bearer"},
		},
		{
			name: "a long value of another header", headers: map[string]string{"X-Scope-OrgID": "production-east"},
			want: []string{"X-Scope-OrgID=production-east"}, wantNot: []string{"production-east"},
		},
		{
			name: "a scheme and a value of another header", headers: map[string]string{"x-custom": "Bearer abc"},
			want: []string{"x-custom=Bearer abc"}, wantNot: []string{"abc", "Bearer abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := headerSecrets(tt.headers)
			for _, want := range tt.want {
				assert.Contains(t, got, want)
			}
			for _, not := range tt.wantNot {
				assert.NotContains(t, got, not)
			}
			assert.Contains(t, got, FormatHeaders(tt.headers), "the whole serialised string")
		})
	}
}

// What is registered masks the token, in each form it is printed, and leaves
// ordinary output alone.
func TestRegisteredHeadersMask(t *testing.T) {
	t.Setenv(envHeaders, "X-Scope-OrgID=nvke,x-flag=true,authorization=Bearer%20example-token")

	RegisterHeaderSecrets(HeadersFromEnv())

	for _, text := range []string{
		"X-Scope-OrgID=nvke",
		"invalid token example-token",
		"Bearer example-token",
		"OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20example-token",
	} {
		got := redact.Text(text)
		assert.NotContains(t, got, "example-token", text)
		assert.Contains(t, got, redact.Placeholder, text)
	}
	for _, text := range []string{`"ready": true`, "namespace nvke is ready", "bearer of the news"} {
		assert.Equal(t, text, redact.Text(text))
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// nilReceiverError and nilReceiverStringer read their receiver, so calling them
// on a typed nil panics; fmt prints such a value as <nil>.
type nilReceiverError struct{ msg string }

func (e *nilReceiverError) Error() string { return e.msg }

type nilReceiverStringer struct{ msg string }

func (s *nilReceiverStringer) String() string { return s.msg }

// A registered credential, such as an OTLP header value, is masked wherever the
// run sends it: the log records and span errors it exports, and the export
// warning, which carries the collector's own rejection to the run's logger.
func TestRunMasksRegisteredCredentials(t *testing.T) {
	t.Run("exported records and spans, registered by EnableOTLP", func(t *testing.T) {
		const token = "exported-token-77aa3b19"
		collector := newFakeCollector(t)
		run := NewRun("dev")
		require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true, Headers: map[string]string{"authorization": "Bearer " + token}}))
		run.Start(context.Background(), "nvkectl get clusters")
		logger := slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, &slog.HandlerOptions{ReplaceAttr: redact.ReplaceAttr})))
		logger.Info("denied with "+token,
			"plain", "Bearer "+token,
			"error", errors.New("quoted "+token),
			slog.Group("g", slog.String("inner", token)),
			"count", 3,
			"bytes", []byte(token),
			"list", []string{"a", token},
			"map", map[string]string{"k": token},
			"struct", struct{ Token string }{token},
			"nilerr", (*nilReceiverError)(nil),
			"nilstringer", (*nilReceiverStringer)(nil),
		)
		logger.With("bound", token).Warn("with attrs")
		run.Finish(errors.New("collector said invalid token " + token))

		records, spans := collector.received(), collector.receivedSpans()
		require.NotEmpty(t, records)
		require.NotEmpty(t, spans)
		var all strings.Builder
		for _, r := range records {
			all.WriteString(r.String())
		}
		for _, s := range spans {
			all.WriteString(s.String())
		}
		assert.NotContains(t, all.String(), token)
		assert.Contains(t, all.String(), redact.Placeholder)
		assert.Contains(t, all.String(), "denied with <redacted>", "the message is kept, masked")
		assert.Contains(t, all.String(), "collector said invalid token <redacted>", "the span's status and its error event")
	})

	t.Run("an HTTP client span, from the transport error and the URL", func(t *testing.T) {
		const token = "transport-token-91c2d3e4"
		RegisterHeaderSecrets(map[string]string{"authorization": "Bearer " + token})
		collector := newFakeCollector(t)
		run := NewRun("dev")
		require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
		ctx := run.Start(context.Background(), "nvkectl get clusters")
		fail := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("proxy refused " + token)
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://controller.example/clusters?token="+token, nil)
		require.NoError(t, err)
		_, err = (&http.Client{Transport: Transport(fail)}).Do(req) //nolint:bodyclose // the transport returns no response
		require.Error(t, err)
		run.Finish(nil)

		spans := collector.receivedSpans()
		require.Len(t, spans, 2, "the root span and the HTTP client span")
		var all strings.Builder
		for _, span := range spans {
			all.WriteString(span.String())
		}
		assert.NotContains(t, all.String(), token)
		assert.Contains(t, all.String(), "proxy refused <redacted>", "the surrounding message is kept")
	})

	t.Run("export warning", func(t *testing.T) {
		const token = "warning-token-2d3e4f50"
		RegisterHeaderSecrets(map[string]string{"authorization": "Bearer " + token})
		stderr := &lockedBuffer{}
		run := NewRun("dev")
		run.Start(context.Background(), "nvkectl get clusters")
		run.warnLogger.Store(slog.New(slog.NewTextHandler(stderr, nil)))

		run.warnExport(errors.New("rpc error: bad " + token))

		assert.NotContains(t, stderr.String(), token)
		assert.Contains(t, stderr.String(), redact.Placeholder)
	})
}
