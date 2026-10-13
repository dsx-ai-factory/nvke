// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package otelx initializes OpenTelemetry tracing for cloud-workflow-engine.
package otelx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/version"
)

// ShutdownFunc flushes and shuts down the tracer provider. It is safe to call
// even when Init returned a no-op (disabled) setup.
type ShutdownFunc func(context.Context) error

// Init configures the global TracerProvider when tracing is enabled. Spans go
// to cfg.Endpoint, or, when that is empty, to the OTLP endpoint the environment
// sets, which is how the OpenTelemetry Operator's inject-sdk supplies a
// localhost sidecar. With neither, Init is a no-op so local runs do not dial a
// collector.
func Init(ctx context.Context, cfg config.TracingConfig, logger *slog.Logger) (ShutdownFunc, error) {
	noop := ShutdownFunc(func(context.Context) error { return nil })

	if !cfg.Enabled {
		logger.Info("OpenTelemetry tracing disabled")
		return noop, nil
	}

	target, ok := resolveOTLPTarget(cfg, os.Getenv)
	if !ok {
		logger.Info("OpenTelemetry tracing enabled, but neither tracing.endpoint nor the environment sets an OTLP endpoint; skipping exporter setup")
		return noop, nil
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "cloud-workflow-engine"
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version.Version),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return noop, fmt.Errorf("create OpenTelemetry resource: %w", err)
	}

	// The SDK also reads OTEL_EXPORTER_OTLP_* itself, and an endpoint's scheme
	// sets its transport security. Passing the resolved endpoint as a URL sets
	// both, so an injected http:// endpoint cannot downgrade a secure
	// tracing.endpoint, and an endpoint the SDK cannot parse as a URL, such as
	// a bare IP:port, still reaches the exporter.
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(target.url()))
	if err != nil {
		return noop, fmt.Errorf("create OTLP gRPC trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("OpenTelemetry tracing initialized",
		"endpoint", target.endpoint,
		"endpointFromEnvironment", target.fromEnv,
		"insecure", target.insecure,
		"serviceName", serviceName,
	)

	return tp.Shutdown, nil
}

// otlpTarget is where the exporter sends spans.
type otlpTarget struct {
	endpoint string
	// fromEnv records that the endpoint came from the environment rather than
	// tracing.endpoint.
	fromEnv  bool
	insecure bool
}

// url returns the endpoint with the scheme its transport security implies.
func (t otlpTarget) url() string {
	scheme := "https://"
	if t.insecure {
		scheme = "http://"
	}
	return scheme + stripOTLPScheme(t.endpoint)
}

// resolveOTLPTarget prefers tracing.endpoint over the environment, and
// tracing.insecure decides the transport for it. An endpoint from the
// environment takes its transport from its own scheme, then from
// OTEL_EXPORTER_OTLP_(TRACES_)INSECURE, and only then from tracing.insecure.
func resolveOTLPTarget(cfg config.TracingConfig, getenv func(string) string) (otlpTarget, bool) {
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		return otlpTarget{endpoint: endpoint, insecure: cfg.Insecure}, true
	}
	endpoint := firstSet(getenv, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return otlpTarget{}, false
	}
	insecure := cfg.Insecure
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		insecure = false
	case strings.HasPrefix(endpoint, "http://"):
		insecure = true
	default:
		if set, err := strconv.ParseBool(firstSet(getenv, "OTEL_EXPORTER_OTLP_TRACES_INSECURE", "OTEL_EXPORTER_OTLP_INSECURE")); err == nil {
			insecure = set
		}
	}
	return otlpTarget{endpoint: endpoint, fromEnv: true, insecure: insecure}, true
}

// firstSet returns the first non-empty value among the named variables.
func firstSet(getenv func(string) string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// stripOTLPScheme removes an optional http:// or https:// prefix so
// otlptracegrpc.WithEndpoint receives host:port as required.
func stripOTLPScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	return endpoint
}
