// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// requireTraceContext checks that Init left the global tracer able to mint
// valid trace IDs and the propagator able to carry them.
func requireTraceContext(t *testing.T) {
	t.Helper()
	_, span := otel.Tracer("otelx-test").Start(context.Background(), "probe")
	defer span.End()
	require.True(t, span.SpanContext().IsValid(), "spans get real trace and span IDs")
	require.True(t, span.SpanContext().TraceID().IsValid())
	require.Contains(t, otel.GetTextMapPropagator().Fields(), "traceparent")
}

// restoreTracerGlobals puts back the global provider and propagator when the
// test ends, because Init replaces them.
func restoreTracerGlobals(t *testing.T) {
	t.Helper()
	provider, propagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(provider)
		otel.SetTextMapPropagator(propagator)
	})
}

// The tests below are not parallel: Init sets the global provider, and the
// endpoint can come from the process environment.

func TestInitDisabledStillCreatesTraceContext(t *testing.T) {
	restoreTracerGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector.example.test:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: false}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	requireTraceContext(t)
	require.NoError(t, shutdown(context.Background()))
}

func TestInitEnabledWithoutEndpointCreatesLocalSpans(t *testing.T) {
	const helperEnv = "NVKE_TEST_OTEL_INIT_WITHOUT_EXPORTER"
	if os.Getenv(helperEnv) == "1" {
		shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: true, Endpoint: ""}, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		spanCtx, span := otel.Tracer("test").Start(context.Background(), "local")
		require.True(t, span.SpanContext().IsValid())
		require.Contains(t, Capture(spanCtx)["traceparent"], span.SpanContext().TraceID().String())
		span.End()
		require.NoError(t, shutdown(context.Background()))
		return
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInitEnabledWithoutEndpointCreatesLocalSpans$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "subprocess failed:\n%s", output)
}

func TestInitWithEndpointCreatesTraceContext(t *testing.T) {
	restoreTracerGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	// The batcher dials lazily, so an unreachable endpoint still initializes.
	cfg := config.TracingConfig{Enabled: true, Endpoint: "127.0.0.1:1", Insecure: true}
	shutdown, err := Init(context.Background(), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	requireTraceContext(t)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(shutdownCtx) // flushing to the unreachable endpoint may time out
}

func TestNewResourceMalformedEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "badentry,k=v")

	t.Run("export off keeps the valid entries and warns", func(t *testing.T) {
		var logs bytes.Buffer
		res, err := newResource(context.Background(), "svc", false, slog.New(slog.NewTextHandler(&logs, nil)))
		require.NoError(t, err)
		attrs := res.Set()
		value, ok := attrs.Value("k")
		require.True(t, ok, "the entry that parsed is kept")
		require.Equal(t, "v", value.AsString())
		value, ok = attrs.Value("service.name")
		require.True(t, ok)
		require.Equal(t, "svc", value.AsString())
		require.Contains(t, logs.String(), "level=WARN")
		require.Contains(t, logs.String(), "badentry")
	})

	t.Run("export on fails", func(t *testing.T) {
		_, err := newResource(context.Background(), "svc", true, slog.New(slog.DiscardHandler))
		require.ErrorIs(t, err, resource.ErrPartialResource)
	})
}

func TestInitMalformedResourceEnvironment(t *testing.T) {
	restoreTracerGlobals(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "badentry")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	// Export off, and enabled without an endpoint: the controller still starts.
	for _, cfg := range []config.TracingConfig{{Enabled: false}, {Enabled: true}} {
		shutdown, err := Init(context.Background(), cfg, slog.New(slog.DiscardHandler))
		require.NoError(t, err, "%+v", cfg)
		requireTraceContext(t)
		require.NoError(t, shutdown(context.Background()))
	}

	// Export on: the malformed value is refused.
	_, err := Init(context.Background(), config.TracingConfig{Enabled: true, Endpoint: "127.0.0.1:1", Insecure: true}, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, resource.ErrPartialResource)
}

func TestExportTarget(t *testing.T) {
	t.Parallel()

	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.example.test:4317"}
	getenv := func(name string) string { return env[name] }
	tests := []struct {
		name       string
		cfg        config.TracingConfig
		getenv     func(string) string
		wantExport bool
	}{
		{name: "disabled does not export even with an endpoint", cfg: config.TracingConfig{Endpoint: "c.example.test:4317"}, getenv: getenv},
		{name: "disabled does not export an environment endpoint", cfg: config.TracingConfig{}, getenv: getenv},
		{name: "enabled without an endpoint does not export", cfg: config.TracingConfig{Enabled: true}, getenv: func(string) string { return "" }},
		{name: "enabled with tracing.endpoint exports", cfg: config.TracingConfig{Enabled: true, Endpoint: "c.example.test:4317"}, getenv: func(string) string { return "" }, wantExport: true},
		{name: "enabled with an environment endpoint exports", cfg: config.TracingConfig{Enabled: true}, getenv: getenv, wantExport: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, export := exportTarget(tt.cfg, tt.getenv, slog.New(slog.DiscardHandler))
			require.Equal(t, tt.wantExport, export)
		})
	}
}

func TestStripOTLPScheme(t *testing.T) {
	t.Parallel()

	require.Equal(t, "localhost:4317", stripOTLPScheme("http://localhost:4317"))
	require.Equal(t, "localhost:4317", stripOTLPScheme("https://localhost:4317"))
	require.Equal(t, "localhost:4317", stripOTLPScheme("localhost:4317"))
}

func TestHTTPClientWrapsTransport(t *testing.T) {
	t.Parallel()

	client := HTTPClient(nil)
	require.NotNil(t, client)
	require.NotNil(t, client.Transport)
}

func TestResolveOTLPTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cfg    config.TracingConfig
		env    map[string]string
		want   otlpTarget
		wantOK bool
	}{
		{
			name:   "file endpoint wins over the environment",
			cfg:    config.TracingConfig{Endpoint: "collector.example.test:4317", Insecure: true},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317"},
			want:   otlpTarget{endpoint: "collector.example.test:4317", insecure: true},
			wantOK: true,
		},
		{
			name:   "operator-injected endpoint, with the file's insecure",
			cfg:    config.TracingConfig{Insecure: true},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317"},
			want:   otlpTarget{endpoint: "localhost:4317", fromEnv: true, insecure: true},
			wantOK: true,
		},
		{
			name:   "traces-specific endpoint is preferred",
			cfg:    config.TracingConfig{},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "localhost:4318"},
			want:   otlpTarget{endpoint: "localhost:4318", fromEnv: true},
			wantOK: true,
		},
		{
			name:   "the environment's insecure setting wins over the file's",
			cfg:    config.TracingConfig{Insecure: true},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.example.test:4317", "OTEL_EXPORTER_OTLP_INSECURE": "false"},
			want:   otlpTarget{endpoint: "collector.example.test:4317", fromEnv: true},
			wantOK: true,
		},
		{
			name:   "an unparsable insecure setting falls back to the file's",
			cfg:    config.TracingConfig{Insecure: true},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.example.test:4317", "OTEL_EXPORTER_OTLP_INSECURE": "maybe"},
			want:   otlpTarget{endpoint: "collector.example.test:4317", fromEnv: true, insecure: true},
			wantOK: true,
		},
		{
			name:   "an https endpoint from the environment stays secure",
			cfg:    config.TracingConfig{Insecure: true},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.test:4317"},
			want:   otlpTarget{endpoint: "https://collector.example.test:4317", fromEnv: true},
			wantOK: true,
		},
		{
			name:   "an http endpoint from the environment is plaintext",
			cfg:    config.TracingConfig{},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317", "OTEL_EXPORTER_OTLP_INSECURE": "false"},
			want:   otlpTarget{endpoint: "http://localhost:4317", fromEnv: true, insecure: true},
			wantOK: true,
		},
		{
			name:   "a secure file endpoint ignores the environment's transport",
			cfg:    config.TracingConfig{Endpoint: "collector.example.test:4317"},
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			want:   otlpTarget{endpoint: "collector.example.test:4317"},
			wantOK: true,
		},
		{
			name: "no endpoint anywhere",
			cfg:  config.TracingConfig{Insecure: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := resolveOTLPTarget(tt.cfg, func(name string) string { return tt.env[name] })
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOTLPTargetURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target otlpTarget
		want   string
	}{
		{name: "secure", target: otlpTarget{endpoint: "collector.example.test:4317"}, want: "https://collector.example.test:4317"},
		{name: "insecure", target: otlpTarget{endpoint: "localhost:4317", insecure: true}, want: "http://localhost:4317"},
		{name: "a bare IP and port", target: otlpTarget{endpoint: "10.0.0.1:4317", insecure: true}, want: "http://10.0.0.1:4317"},
		{name: "the scheme follows insecure", target: otlpTarget{endpoint: "http://collector.example.test:4317"}, want: "https://collector.example.test:4317"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.target.url())
		})
	}
}
