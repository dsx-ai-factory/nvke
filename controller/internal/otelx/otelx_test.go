// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

func TestInitDisabledIsNoop(t *testing.T) {
	t.Parallel()

	shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: false}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
}

// Not parallel: the endpoint can come from the process environment.
func TestInitEnabledWithoutEndpointIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: true, Endpoint: ""}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
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
