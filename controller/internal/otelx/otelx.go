// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package otelx initializes OpenTelemetry trace context and span export for
// cloud-workflow-engine.
package otelx

import (
	"context"
	"errors"
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
// even when Init returned an error.
type ShutdownFunc func(context.Context) error

// Init installs the global TracerProvider and the W3C trace context
// propagator, so every request and workflow gets a trace ID whatever the
// configuration.
//
// Spans are exported only when cfg.Enabled is true and an endpoint is set:
// cfg.Endpoint, or the OTEL_EXPORTER_OTLP_* variables the OpenTelemetry
// Operator injects. Otherwise they are discarded when they end.
func Init(ctx context.Context, cfg config.TracingConfig, logger *slog.Logger) (ShutdownFunc, error) {
	noop := ShutdownFunc(func(context.Context) error { return nil })

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "cloud-workflow-engine"
	}

	target, export := exportTarget(cfg, os.Getenv, logger)
	res, err := newResource(ctx, serviceName, export, logger)
	if err != nil {
		return noop, err
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if export {
		// The SDK also reads OTEL_EXPORTER_OTLP_* itself, and an endpoint's scheme
		// sets its transport security. Passing the resolved endpoint as a URL sets
		// both, so an injected http:// endpoint cannot downgrade a secure
		// tracing.endpoint, and an endpoint the SDK cannot parse as a URL, such as
		// a bare IP:port, still reaches the exporter.
		exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(target.url()))
		if err != nil {
			return noop, fmt.Errorf("create OTLP gRPC trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("OpenTelemetry trace context initialized",
		"export", export,
		"endpoint", target.endpoint,
		"endpointFromEnvironment", target.fromEnv,
		"insecure", target.insecure,
		"serviceName", serviceName,
	)

	return tp.Shutdown, nil
}

// newResource builds the resource attached to every span. A malformed
// OTEL_RESOURCE_ATTRIBUTES entry is an error when export is true. Otherwise it
// is logged as a warning and the entries that parsed are kept.
func newResource(ctx context.Context, serviceName string, export bool, logger *slog.Logger) (*resource.Resource, error) {
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
		if export || !errors.Is(err, resource.ErrPartialResource) {
			return nil, fmt.Errorf("create OpenTelemetry resource: %w", err)
		}
		logger.Warn("OpenTelemetry resource attributes are partly invalid; keeping the valid ones", "error", err)
	}
	return res, nil
}

// exportTarget returns the endpoint spans are exported to, and false when
// export is disabled or no endpoint is set. It logs a warning when export is
// enabled without an endpoint, since the spans then go nowhere.
func exportTarget(cfg config.TracingConfig, getenv func(string) string, logger *slog.Logger) (otlpTarget, bool) {
	if !cfg.Enabled {
		logger.Info("OpenTelemetry span export disabled; trace IDs are still created")
		return otlpTarget{}, false
	}
	target, ok := resolveOTLPTarget(cfg, getenv)
	if !ok {
		logger.Warn("tracing.enabled is true but no OTLP endpoint is set, so spans will not be exported; set tracing.endpoint or OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	return target, ok
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
