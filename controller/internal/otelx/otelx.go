// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package otelx initializes OpenTelemetry tracing for cloud-workflow-engine.
package otelx

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/version"
)

// ShutdownFunc flushes and shuts down the tracer provider. It is safe to call
// even when Init returned a no-op (disabled) setup.
type ShutdownFunc func(context.Context) error

// Init configures the global TracerProvider when tracing is enabled and an
// OTLP endpoint is set. Prefer an operator-injected OTEL_EXPORTER_OTLP_ENDPOINT
// (typically a localhost sidecar) over hardcoding collector addresses.
// When disabled or the endpoint is empty, Init is a no-op so local runs do not
// dial a collector.
func Init(ctx context.Context, cfg config.TracingConfig, logger *slog.Logger) (ShutdownFunc, error) {
	noop := ShutdownFunc(func(context.Context) error { return nil })

	if !cfg.Enabled {
		logger.Info("OpenTelemetry tracing disabled")
		return noop, nil
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		logger.Info("OpenTelemetry tracing enabled but OTEL_EXPORTER_OTLP_ENDPOINT is empty; skipping exporter setup (expect operator inject-sdk or an explicit endpoint)")
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

	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(stripOTLPScheme(endpoint)),
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
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
		"endpoint", endpoint,
		"insecure", cfg.Insecure,
		"serviceName", serviceName,
	)

	return tp.Shutdown, nil
}

// stripOTLPScheme removes an optional http:// or https:// prefix so
// otlptracegrpc.WithEndpoint receives host:port as required.
func stripOTLPScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	return endpoint
}
