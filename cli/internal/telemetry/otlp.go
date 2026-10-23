// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	envEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envInsecure = "OTEL_EXPORTER_OTLP_INSECURE"
	envHeaders  = "OTEL_EXPORTER_OTLP_HEADERS"

	// flushTimeout bounds how long Finish waits for buffered records to reach
	// the endpoint, so an unreachable endpoint cannot hang a command.
	flushTimeout = 2 * time.Second

	serviceName = "nvkectl"
)

// OTLPConfig is where a run exports its telemetry over OTLP/gRPC.
type OTLPConfig struct {
	// Endpoint is the collector's host:port.
	Endpoint string
	// Insecure sends without TLS.
	Insecure bool
	// Headers are sent on every export request, for instance an
	// authorization token. They are never logged.
	Headers map[string]string
}

// OTLPConfigFromEnv reads OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_INSECURE
// and OTEL_EXPORTER_OTLP_HEADERS, and no other OTLP variable. The endpoint is a
// host:port or a URL. The http scheme sets Insecure and any other scheme clears
// it; only an endpoint with no scheme reads OTEL_EXPORTER_OTLP_INSECURE. It
// returns false when no endpoint is set, and an error that does not contain the
// endpoint when the endpoint cannot be parsed or contains credentials.
func OTLPConfigFromEnv() (OTLPConfig, bool, error) {
	// Variables for one signal, such as OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, are not
	// read: this endpoint and these headers serve every signal nvkectl exports.
	raw := os.Getenv(envEndpoint)
	if raw == "" {
		return OTLPConfig{}, false, nil
	}
	cfg := OTLPConfig{Endpoint: raw, Headers: parseHeaders(os.Getenv(envHeaders))}
	target := raw
	if !strings.Contains(raw, "://") {
		target = "//" + raw
	}
	u, err := url.Parse(target)
	// The error names the variable and not its value, which may hold credentials.
	if err != nil || u.Host == "" || u.User != nil {
		return OTLPConfig{}, false, fmt.Errorf("invalid %s: want host:port or a URL with a host and no credentials", envEndpoint)
	}
	cfg.Endpoint = u.Host
	switch u.Scheme {
	case "http":
		cfg.Insecure = true
	case "":
		cfg.Insecure, _ = strconv.ParseBool(os.Getenv(envInsecure))
	}
	return cfg, true, nil
}

// parseHeaders parses the "key=value,key=value" form of OTEL_EXPORTER_OTLP_HEADERS.
// Values are percent-decoded; an entry without "=" is ignored.
func parseHeaders(raw string) map[string]string {
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if decoded, err := url.PathUnescape(strings.TrimSpace(value)); err == nil {
			value = decoded
		}
		headers[key] = strings.TrimSpace(value)
	}
	return headers
}

// EnableOTLP makes the run export its log records to cfg. It must be called
// before LogHandler and returns an error afterwards. Creating the exporter
// contacts nothing: an unreachable endpoint is reported by a single warning,
// never as an error.
func (run *Run) EnableOTLP(cfg OTLPConfig) error {
	if cfg.Endpoint == "" {
		return errors.New("OTLP endpoint is empty")
	}
	if run.handlerBuilt {
		return errors.New("OTLP export must be enabled before the log handler is built")
	}
	// Every setting is explicit: the exporter reads its own OTEL_EXPORTER_OTLP_*
	// variables, including the logs-only ones, for any that are left unset.
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	}
	headers := cfg.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithTLSCredentials(creds),
		otlploggrpc.WithHeaders(headers),
		otlploggrpc.WithCompressor("none"),
		otlploggrpc.WithTimeout(flushTimeout),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 200 * time.Millisecond,
			MaxInterval:     500 * time.Millisecond,
			MaxElapsedTime:  flushTimeout / 2,
		}),
	}
	exporter, err := otlploggrpc.New(context.Background(), opts...)
	if err != nil {
		return fmt.Errorf("creating the OTLP log exporter: %w", err)
	}
	run.logProvider = sdklog.NewLoggerProvider(
		sdklog.WithResource(run.resource()),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(
			&guardedExporter{Exporter: exporter, warn: run.warnExport},
			sdklog.WithExportTimeout(flushTimeout),
		)),
	)
	return nil
}

// resource describes nvkectl to the backend, for every signal the run exports.
func (run *Run) resource() *resource.Resource {
	return resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", run.version),
	)
}

// otlpHandler returns the handler that sends records to the OTLP endpoint, or
// nil if EnableOTLP was not called.
func (run *Run) otlpHandler() slog.Handler {
	run.handlerBuilt = true
	if run.logProvider == nil {
		return nil
	}
	return otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(run.logProvider))
}

// flushLogs sends the buffered records, waiting at most flushTimeout, and shuts
// the exporter down. A failure is reported as the run's single export warning.
func (run *Run) flushLogs() {
	if run.logProvider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := run.logProvider.Shutdown(ctx); err != nil {
		run.warnExport(err)
	}
}

// warnExport reports, once per run, that telemetry could not be exported. The
// warning goes to the run's own logger and never to the endpoint that failed.
func (run *Run) warnExport(err error) {
	run.exportWarnOnce.Do(func() {
		logger := run.warnLogger.Load()
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("could not export telemetry to the OTLP endpoint; the command is not affected", "error", err.Error())
	})
}

// guardedExporter is an sdklog.Exporter whose Export never returns an error. It
// passes an export failure to warn.
type guardedExporter struct {
	sdklog.Exporter
	warn func(error)
}

// Export implements sdklog.Exporter.
func (e *guardedExporter) Export(ctx context.Context, records []sdklog.Record) error {
	// An error returned here reaches the OpenTelemetry error handler, which
	// prints it for every batch.
	if err := e.Exporter.Export(ctx, records); err != nil {
		e.warn(err)
	}
	return nil
}
