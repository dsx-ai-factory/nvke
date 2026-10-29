// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
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
// and OTEL_EXPORTER_OTLP_HEADERS, and no other OTLP variable. The endpoint takes
// the forms ParseEndpoint accepts. The http scheme sets Insecure and the https
// scheme clears it; only an endpoint with no scheme reads
// OTEL_EXPORTER_OTLP_INSECURE. It returns false when no endpoint is set, and an
// error that does not contain the endpoint when ParseEndpoint rejects it.
func OTLPConfigFromEnv() (OTLPConfig, bool, error) {
	// Variables for one signal, such as OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, are not
	// read: this endpoint and these headers serve every signal nvkectl exports.
	raw := os.Getenv(envEndpoint)
	if raw == "" {
		return OTLPConfig{}, false, nil
	}
	endpoint, err := ParseEndpoint(raw)
	if err != nil {
		// The error names the variable and not its value, which may hold credentials.
		return OTLPConfig{}, false, fmt.Errorf("%s: %w", envEndpoint, err)
	}
	cfg := OTLPConfig{Endpoint: endpoint.Host, Insecure: endpoint.Insecure, Headers: HeadersFromEnv()}
	if !endpoint.hasScheme {
		cfg.Insecure, _ = strconv.ParseBool(os.Getenv(envInsecure))
	}
	return cfg, true, nil
}

// Endpoint is an OTLP/gRPC endpoint as ParseEndpoint returns it.
type Endpoint struct {
	// Host is the host:port, or the host alone, to dial.
	Host string
	// Insecure is true for the http scheme, which sends without TLS.
	Insecure bool
	// HTTPS is true for the https scheme, which always uses TLS.
	HTTPS bool
	// hasScheme is true when the endpoint had a scheme. Without one, the caller
	// decides Insecure.
	hasScheme bool
}

// ParseEndpoint parses an OTLP/gRPC endpoint given as a host:port or an http or
// https URL. An IPv6 address has to be in brackets. It returns an error that
// does not contain the endpoint, which may hold credentials, when the endpoint
// is empty or cannot be parsed, has another scheme, or has credentials, a path,
// a query or a fragment.
func ParseEndpoint(raw string) (Endpoint, error) {
	target := raw
	if !strings.Contains(raw, "://") {
		target = "//" + raw
	}
	u, err := url.Parse(target)
	if err != nil || !isOTLPAddress(u) {
		return Endpoint{}, errors.New("invalid OTLP endpoint: want host:port or an http or https URL with a host, " +
			"an IPv6 address in brackets, a port if there is a colon, no credentials and no path")
	}
	return Endpoint{Host: u.Host, Insecure: u.Scheme == "http", HTTPS: u.Scheme == "https", hasScheme: u.Scheme != ""}, nil
}

// isOTLPAddress reports whether u is an OTLP/gRPC address: a host and a port with
// no credentials, path or query. A path or a query marks an OTLP/HTTP URL.
func isOTLPAddress(u *url.URL) bool {
	switch u.Scheme {
	case "", "http", "https":
	default:
		return false
	}
	// An IPv6 address without brackets is ambiguous: its last group could be a port.
	if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
		return false
	}
	// An empty host (":4317") or an empty port ("host:") is no address.
	if u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	return u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// HeadersFromEnv reads the headers to send on every export request from
// OTEL_EXPORTER_OTLP_HEADERS.
func HeadersFromEnv() map[string]string {
	return parseHeaders(os.Getenv(envHeaders))
}

// sensitiveHeaderMarkers are the parts of a header name that mark its value as a
// credential, such as authorization, x-api-key, x-honeycomb-team or uptrace-dsn.
var sensitiveHeaderMarkers = []string{"auth", "token", "key", "secret", "password", "credential", "cookie", "team", "dsn"}

func isSensitiveHeader(name string) bool {
	name = strings.ToLower(name)
	return slices.ContainsFunc(sensitiveHeaderMarkers, func(marker string) bool {
		return strings.Contains(name, marker)
	})
}

// minBareSecretLength is the shortest value masked on its own. A shorter one,
// such as `true` or a scheme's one-letter token, would corrupt ordinary output
// wherever it appears.
const minBareSecretLength = 8

// RegisterHeaderSecrets registers the secrets derived from headers with the
// redact package, so that nvkectl masks them wherever it prints, saves or
// exports text.
func RegisterHeaderSecrets(headers map[string]string) {
	for _, secret := range headerSecrets(headers) {
		redact.RegisterSecret(secret)
	}
}

// headerSecrets returns what RegisterHeaderSecrets registers: the whole encoded
// set and each `name=value` pair, as written and percent-encoded. The value of a
// header with a credential name, and the credential of a `scheme credential`
// value, are registered on their own too when they have at least
// minBareSecretLength characters. Other values are not, because a value such as
// a tenant name also appears in ordinary output.
func headerSecrets(headers map[string]string) []string {
	secrets := []string{FormatHeaders(headers)}
	for name, value := range headers {
		if value == "" {
			continue
		}
		escaped := escapeHeaderValue(value)
		secrets = append(secrets, name+"="+value, name+"="+escaped)
		if !isSensitiveHeader(name) {
			continue
		}
		if len(value) >= minBareSecretLength {
			secrets = append(secrets, value, escaped)
		}
		if fields := strings.Fields(value); len(fields) > 1 {
			if credential := fields[len(fields)-1]; len(credential) >= minBareSecretLength {
				secrets = append(secrets, credential)
			}
		}
	}
	return secrets
}

// escapeHeaderValue percent-encodes a header value as url.PathEscape does, and
// "&" too, which JSON output such as `kubectl cluster-info dump` writes as
// \u0026. The SDK's header parser decodes it.
func escapeHeaderValue(value string) string {
	return strings.ReplaceAll(url.PathEscape(value), "&", "%26")
}

// FormatHeaders returns headers in the "key=value,key=value" form of
// OTEL_EXPORTER_OTLP_HEADERS, sorted by key, with each value escaped by
// escapeHeaderValue so that it survives being read back. It returns "" for no
// headers. A header name that holds a comma or an equals sign cannot be
// represented.
func FormatHeaders(headers map[string]string) string {
	pairs := make([]string, 0, len(headers))
	for _, key := range slices.Sorted(maps.Keys(headers)) {
		pairs = append(pairs, key+"="+escapeHeaderValue(headers[key]))
	}
	return strings.Join(pairs, ",")
}

// RejectLoopback returns an error if endpoint, a host:port or a URL, names the
// local machine: localhost, a name under .localhost, or a loopback or unspecified
// address. The temporary controller in the bootstrap cluster cannot reach such an
// endpoint. The host is not resolved. An endpoint ParseEndpoint rejects gives its
// error.
func RejectLoopback(endpoint string) error {
	parsed, err := ParseEndpoint(endpoint)
	if err != nil {
		return err
	}
	return RejectLoopbackEndpoint(endpoint, parsed)
}

// RejectLoopbackEndpoint is RejectLoopback for an endpoint that ParseEndpoint
// already parsed into parsed. raw is the endpoint as given, which the error
// names.
func RejectLoopbackEndpoint(raw string, parsed Endpoint) error {
	host := strings.TrimSuffix((&url.URL{Host: parsed.Host}).Hostname(), ".")
	// ParseAddr accepts a zone ("::1%lo0") and Unmap an IPv4-mapped address.
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr = addr.Unmap(); addr.IsLoopback() || addr.IsUnspecified() {
			return fmt.Errorf("OTLP endpoint %q is a loopback address, which the temporary controller in the bootstrap cluster cannot reach", raw)
		}
		return nil
	}
	if isLocalName(host) {
		return fmt.Errorf("OTLP endpoint %q is a loopback address, which the temporary controller in the bootstrap cluster cannot reach", raw)
	}
	return nil
}

// isLocalName reports whether host is localhost or a name under .localhost,
// which resolve to the local machine.
func isLocalName(host string) bool {
	host = strings.ToLower(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
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

// EnableOTLP makes the run export its log records and spans to cfg. It may be
// called before or after Start, but must be called before LogHandler and
// returns an error afterwards, or if it was already called. Creating the
// exporters contacts nothing: an unreachable endpoint is reported by a single
// warning, never as an error.
func (run *Run) EnableOTLP(cfg OTLPConfig) error {
	if cfg.Endpoint == "" {
		return errors.New("OTLP endpoint is empty")
	}
	if run.handlerBuilt {
		return errors.New("OTLP export must be enabled before the log handler is built")
	}
	if run.otlpEnabled {
		return errors.New("OTLP export is already enabled")
	}
	// The exporters keep an endpoint from the environment when they reject the
	// URL they are given, so the URL is checked first.
	target, err := endpointURL(cfg)
	if err != nil {
		return err
	}
	// Register the headers before anything can print them.
	RegisterHeaderSecrets(cfg.Headers)
	// Every setting is explicit, so the exporters read none of their own
	// OTEL_EXPORTER_OTLP_* variables for them: an http scheme or
	// OTEL_EXPORTER_OTLP_INSECURE there would switch TLS off. TLS uses the
	// system roots: the exporters still read their certificate variables,
	// ignore them, and the log exporter prints a bad one itself.
	//
	// The span exporter parses OTEL_EXPORTER_OTLP_HEADERS itself and prints a
	// malformed entry, which can be a token, through OpenTelemetry's internal
	// logger, so that logger is discarded. The global error handler is left to
	// its owner.
	otel.SetLogger(logr.Discard())
	logOpts := []otlploggrpc.Option{
		otlploggrpc.WithEndpointURL(target),
		otlploggrpc.WithTLSCredentials(transportCredentials(cfg)),
		otlploggrpc.WithHeaders(headersOrEmpty(cfg)),
		otlploggrpc.WithCompressor("none"),
		otlploggrpc.WithTimeout(flushTimeout),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 200 * time.Millisecond,
			MaxInterval:     500 * time.Millisecond,
			MaxElapsedTime:  flushTimeout / 2,
		}),
	}
	logExporter, err := otlploggrpc.New(context.Background(), logOpts...)
	if err != nil {
		return fmt.Errorf("creating the OTLP log exporter: %w", err)
	}
	processor, err := newSpanProcessor(target, cfg, run.warnExport)
	if err != nil {
		_ = logExporter.Shutdown(context.Background())
		return err
	}
	run.logProvider = sdklog.NewLoggerProvider(
		sdklog.WithResource(run.resource()),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(
			&guardedExporter{Exporter: logExporter, warn: run.warnExport},
			sdklog.WithExportTimeout(flushTimeout),
		)),
	)
	run.mu.Lock()
	if run.tracerProvider != nil {
		// Start already ran; the SDK hands a span to the processors registered when it ends.
		run.tracerProvider.RegisterSpanProcessor(processor)
	} else {
		run.providerOptions = append(run.providerOptions, sdktrace.WithSpanProcessor(processor))
	}
	run.mu.Unlock()
	run.otlpConfig, run.otlpEnabled = cfg, true
	return nil
}

// newSpanProcessor returns the processor that exports spans to the endpoint URL
// target, with cfg's transport security and headers.
func newSpanProcessor(target string, cfg OTLPConfig, warn func(error)) (sdktrace.SpanProcessor, error) {
	// Compression is left to the exporter, which cannot be told "none".
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpointURL(target),
		otlptracegrpc.WithTLSCredentials(transportCredentials(cfg)),
		otlptracegrpc.WithHeaders(headersOrEmpty(cfg)),
		otlptracegrpc.WithTimeout(flushTimeout),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 200 * time.Millisecond,
			MaxInterval:     500 * time.Millisecond,
			MaxElapsedTime:  flushTimeout / 2,
		}),
	}
	exporter, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating the OTLP trace exporter: %w", err)
	}
	return sdktrace.NewBatchSpanProcessor(
		&guardedSpanExporter{SpanExporter: exporter, warn: warn},
		sdktrace.WithExportTimeout(flushTimeout),
	), nil
}

// endpointURL returns cfg's endpoint as a URL whose scheme matches cfg.Insecure.
// It returns an error if the URL does not read back as the same host, which
// leaves the endpoint to the exporters' own environment.
func endpointURL(cfg OTLPConfig) (string, error) {
	scheme := "https"
	if cfg.Insecure {
		scheme = "http"
	}
	target := (&url.URL{Scheme: scheme, Host: cfg.Endpoint}).String()
	if parsed, err := url.Parse(target); err != nil || parsed.Host != cfg.Endpoint || parsed.Hostname() == "" {
		return "", errors.New("OTLP endpoint is not a valid host:port")
	}
	return target, nil
}

// transportCredentials returns TLS, or plaintext when cfg.Insecure is set.
func transportCredentials(cfg OTLPConfig) credentials.TransportCredentials {
	if cfg.Insecure {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}

// headersOrEmpty returns cfg.Headers, or an empty map if it is nil.
func headersOrEmpty(cfg OTLPConfig) map[string]string {
	if cfg.Headers == nil {
		// Not nil, so the exporter does not read OTEL_EXPORTER_OTLP_HEADERS itself.
		return map[string]string{}
	}
	return cfg.Headers
}

// OTLP returns the configuration EnableOTLP accepted, and false if it has not
// been called.
func (run *Run) OTLP() (OTLPConfig, bool) {
	return run.otlpConfig, run.otlpEnabled
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
	if run.logProvider == nil {
		return nil
	}
	return otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(run.logProvider))
}

// shutdownProviders sends the buffered spans and records, waiting at most
// flushTimeout for both together, and releases the exporters. A failure is
// reported as the run's single export warning.
func (run *Run) shutdownProviders(tracerProvider *sdktrace.TracerProvider, logProvider *sdklog.LoggerProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	var wg sync.WaitGroup
	if tracerProvider != nil {
		wg.Go(func() {
			if err := tracerProvider.Shutdown(ctx); err != nil {
				run.warnExport(err)
			}
		})
	}
	if logProvider != nil {
		wg.Go(func() {
			if err := logProvider.Shutdown(ctx); err != nil {
				run.warnExport(err)
			}
		})
	}
	wg.Wait()
}

// warnExport reports, once per run, that telemetry could not be exported. The
// warning goes to the run's own logger and never to the endpoint that failed.
func (run *Run) warnExport(err error) {
	run.exportWarnOnce.Do(func() {
		logger := run.warnLogger.Load()
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("could not export telemetry to the OTLP endpoint; the command is not affected", "error", redact.Text(err.Error()))
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

// guardedSpanExporter is guardedExporter for spans.
type guardedSpanExporter struct {
	sdktrace.SpanExporter
	warn func(error)
}

// ExportSpans implements sdktrace.SpanExporter. It exports the spans with the
// registered credentials masked in their names, statuses, attributes and events.
func (e *guardedSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	masked := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		masked[i] = maskedSpan{span}
	}
	if err := e.SpanExporter.ExportSpans(ctx, masked); err != nil {
		e.warn(err)
	}
	return nil
}

// maskedSpan is a span whose name, status, attributes and events are masked.
type maskedSpan struct{ sdktrace.ReadOnlySpan }

// Name implements sdktrace.ReadOnlySpan.
func (s maskedSpan) Name() string { return redact.Text(s.ReadOnlySpan.Name()) }

// Status implements sdktrace.ReadOnlySpan.
func (s maskedSpan) Status() sdktrace.Status {
	status := s.ReadOnlySpan.Status()
	status.Description = redact.Text(status.Description)
	return status
}

// Attributes implements sdktrace.ReadOnlySpan.
func (s maskedSpan) Attributes() []attribute.KeyValue {
	return maskAttributes(s.ReadOnlySpan.Attributes())
}

// Events implements sdktrace.ReadOnlySpan.
func (s maskedSpan) Events() []sdktrace.Event {
	events := slices.Clone(s.ReadOnlySpan.Events())
	for i := range events {
		events[i].Name = redact.Text(events[i].Name)
		events[i].Attributes = maskAttributes(events[i].Attributes)
	}
	return events
}

// maskAttributes returns attrs with the registered credentials masked in string
// and string-slice values.
func maskAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	masked := make([]attribute.KeyValue, len(attrs))
	for i, kv := range attrs {
		switch kv.Value.Type() {
		case attribute.STRING:
			kv = attribute.String(string(kv.Key), redact.Text(kv.Value.AsString()))
		case attribute.STRINGSLICE:
			values := slices.Clone(kv.Value.AsStringSlice())
			for j := range values {
				values[j] = redact.Text(values[j])
			}
			kv = attribute.StringSlice(string(kv.Key), values)
		}
		masked[i] = kv
	}
	return masked
}
