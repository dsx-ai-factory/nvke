// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

// otlpCollector is an in-process OTLP/gRPC endpoint that keeps the spans it
// receives and the headers they came with.
type otlpCollector struct {
	coltracepb.UnimplementedTraceServiceServer

	addr string
	// reject, when set, is the message every export is refused with.
	reject string

	mu       sync.Mutex
	spans    []*tracepb.Span
	metadata metadata.MD
}

// nonLoopbackIPv4 returns a non-loopback IPv4 address of this machine, so a test
// can pass it to a flag that rejects loopback. With mustConnect, the machine must
// also be able to connect to itself on it. The test is skipped when no address
// qualifies, as a host firewall or VPN can block such connections.
func nonLoopbackIPv4(t *testing.T, mustConnect bool) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() && (!mustConnect || canDialSelf(ipNet.IP.String())) {
				return ipNet.IP.String()
			}
		}
	}
	t.Skip("this machine has no non-loopback IPv4 address it can open a TCP connection to itself on")
	return ""
}

// canDialSelf reports whether a connection to a listener on ip reaches it and
// carries data both ways.
func canDialSelf(ip string) bool {
	lis, err := net.Listen("tcp", ip+":0")
	if err != nil {
		return false
	}
	defer lis.Close()
	go func() {
		if conn, err := lis.Accept(); err == nil {
			_, _ = conn.Write([]byte{1})
			conn.Close()
		}
	}()
	conn, err := net.DialTimeout("tcp", lis.Addr().String(), time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = conn.Read(make([]byte, 1))
	return err == nil
}

// newOTLPCollector starts a collector on a non-loopback address of this
// machine and returns it with that address as host:port.
func newOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()
	return newOTLPCollectorOn(t, nonLoopbackIPv4(t, true), "")
}

// newOTLPCollectorOn starts a collector listening on host, which refuses every
// export with the message reject when that is not empty.
func newOTLPCollectorOn(t *testing.T, host, reject string) *otlpCollector {
	t.Helper()
	lis, err := net.Listen("tcp", host+":0")
	require.NoError(t, err)
	c := &otlpCollector{addr: lis.Addr().String(), reject: reject}
	server := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(server, c)
	collogspb.RegisterLogsServiceServer(server, &otlpLogsSink{reject: reject})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return c
}

// Export implements coltracepb.TraceServiceServer.
func (c *otlpCollector) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if c.reject != "" {
		return nil, status.Error(codes.Unauthenticated, c.reject)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metadata, _ = metadata.FromIncomingContext(ctx)
	for _, resourceSpans := range req.GetResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			c.spans = append(c.spans, scopeSpans.GetSpans()...)
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (c *otlpCollector) received() ([]*tracepb.Span, metadata.MD) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...), c.metadata.Copy()
}

// otlpLogsSink accepts log exports, which the tests here do not inspect. The
// logs and trace services both name their method Export, so they cannot share a type.
type otlpLogsSink struct {
	collogspb.UnimplementedLogsServiceServer
	reject string
}

// Export implements collogspb.LogsServiceServer.
func (s *otlpLogsSink) Export(context.Context, *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if s.reject != "" {
		return nil, status.Error(codes.Unauthenticated, s.reject)
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// An OTLP endpoint that is unreachable or misconfigured never fails a command
// or holds it up. These tests run commands through execute, so none is parallel.
func TestOTLPEndpointNeverFailsTheCommand(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedPort := lis.Addr().String()
	require.NoError(t, lis.Close())

	tests := map[string]string{
		"unreachable":    "http://" + closedPort,
		"unparsable":     "http://",
		"no endpoint":    "",
		"https endpoint": "https://" + closedPort,
	}
	for name, endpoint := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			root := NewRootCmd("test", "test", "test", "test")
			root.AddCommand(&cobra.Command{
				Use: "probe",
				RunE: func(*cobra.Command, []string) error {
					slog.Info("probe ran")
					return nil
				},
			})
			root.SetArgs([]string{"probe"})
			var stderr bytes.Buffer
			root.SetErr(&stderr)

			started := time.Now()
			require.NoError(t, execute(context.Background(), root, "test", &stderr))

			assert.Less(t, time.Since(started), 4*time.Second, "the flush is bounded")
			assert.Regexp(t, traceIDLineRE, stderr.String(), "the command's output is unchanged")
		})
	}
}

// logsCollector is an in-process OTLP/gRPC logs endpoint that keeps the body of
// every record it receives.
type logsCollector struct {
	collogspb.UnimplementedLogsServiceServer

	addr string

	mu     sync.Mutex
	bodies []string
}

func newLogsCollector(t *testing.T) *logsCollector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	collector := &logsCollector{addr: lis.Addr().String()}
	server := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(server, collector)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return collector
}

// Export implements collogspb.LogsServiceServer.
func (c *logsCollector) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resourceLogs := range req.GetResourceLogs() {
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, record := range scopeLogs.GetLogRecords() {
				c.bodies = append(c.bodies, record.GetBody().GetStringValue())
			}
		}
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (c *logsCollector) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// The last record a successful query command logs, "command finished", is
// written after the command returns, so the export must be flushed after it.
// This test runs the command through execute, so it is not parallel.
func TestOTLPExportsTheFinalRecordOfAQueryCommand(t *testing.T) {
	collector := newLogsCollector(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+collector.addr)
	t.Cleanup(func() { verbose = false })

	root := NewRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"version", "--verbose"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	require.NoError(t, execute(context.Background(), root, "test", io.Discard))

	assert.Contains(t, collector.received(), "command finished")
}

// resolveFor parses args as the flags of "bootstrap site" and resolves the OTLP
// configuration for it, without touching the network.
func resolveFor(t *testing.T, args ...string) (cfg telemetry.OTLPConfig, ok, strict bool, err error) {
	t.Helper()
	cmd := newBootstrapSiteCmd()
	require.NoError(t, cmd.ParseFlags(args))
	return resolveOTLP(cmd)
}

func TestResolveOTLPPrecedence(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "other.example:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer x")

	t.Run("the flag wins over the environment and keeps its headers", func(t *testing.T) {
		cfg, ok, strict, err := resolveFor(t, "--otlp-endpoint", "collector.example:4317")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, strict)
		assert.Equal(t, "collector.example:4317", cfg.Endpoint)
		assert.False(t, cfg.Insecure)
		assert.Equal(t, "Bearer x", cfg.Headers["authorization"])
	})

	t.Run("without the flag the environment applies and is not strict", func(t *testing.T) {
		cfg, ok, strict, err := resolveFor(t)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, strict)
		assert.Equal(t, "other.example:4317", cfg.Endpoint)
		assert.Equal(t, "Bearer x", cfg.Headers["authorization"])
	})

	t.Run("an empty endpoint is no endpoint, so the environment applies", func(t *testing.T) {
		cfg, ok, strict, err := resolveFor(t, "--otlp-endpoint", "")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, strict)
		assert.Equal(t, "other.example:4317", cfg.Endpoint)
	})

	t.Run("--otlp-insecure without a non-empty endpoint is a strict error", func(t *testing.T) {
		for _, args := range [][]string{{"--otlp-insecure"}, {"--otlp-endpoint", "", "--otlp-insecure"}} {
			_, ok, strict, err := resolveFor(t, args...)
			require.Error(t, err, "%v", args)
			assert.False(t, ok)
			assert.True(t, strict)
			assert.Contains(t, err.Error(), "--otlp-endpoint")
		}
	})

	t.Run("--otlp-insecure sets Insecure", func(t *testing.T) {
		cfg, ok, _, err := resolveFor(t, "--otlp-endpoint", "collector.example:4317", "--otlp-insecure")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, cfg.Insecure)
	})

	t.Run("--otlp-insecure with an https URL is a strict error", func(t *testing.T) {
		_, ok, strict, err := resolveFor(t, "--otlp-endpoint", "https://collector.example:4317", "--otlp-insecure")
		require.Error(t, err)
		assert.False(t, ok)
		assert.True(t, strict)
		assert.Contains(t, err.Error(), "https")
	})

	t.Run("--otlp-insecure with an http URL is allowed", func(t *testing.T) {
		cfg, ok, _, err := resolveFor(t, "--otlp-endpoint", "http://collector.example:4317", "--otlp-insecure")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, cfg.Insecure)
	})

	t.Run("an http URL gives the host and implies insecure", func(t *testing.T) {
		cfg, ok, _, err := resolveFor(t, "--otlp-endpoint", "http://collector.example:4317")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "collector.example:4317", cfg.Endpoint)
		assert.True(t, cfg.Insecure)
	})

	t.Run("a flag error is strict", func(t *testing.T) {
		_, ok, strict, err := resolveFor(t, "--otlp-endpoint", "localhost:4317")
		require.Error(t, err)
		assert.False(t, ok)
		assert.True(t, strict)
	})

	t.Run("an endpoint the environment would reject is a strict error that does not contain it", func(t *testing.T) {
		_, ok, strict, err := resolveFor(t, "--otlp-endpoint", "https://user:s3cret@collector.example:4317")
		require.Error(t, err)
		assert.False(t, ok)
		assert.True(t, strict)
		assert.Contains(t, err.Error(), "--otlp-endpoint")
		assert.NotContains(t, err.Error(), "s3cret")
	})

	t.Run("the flag does not read OTEL_EXPORTER_OTLP_INSECURE", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
		cfg, ok, _, err := resolveFor(t, "--otlp-endpoint", "collector.example:4317")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, cfg.Insecure)
	})
}

// Any other command gets the warning without the flag, and still succeeds with
// its output unchanged.
func TestOTLPBadEnvironmentWarningOnOtherCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:secret@")
	root := NewRootCmd("test", "test", "test", "test")
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return nil }})
	root.SetArgs([]string{"probe"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	realStderr, realLogger := os.Stderr, slog.Default()
	t.Cleanup(func() { os.Stderr = realStderr; slog.SetDefault(realLogger) })
	os.Stderr = w
	var logs bytes.Buffer
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(&logs, r); close(copied) }()
	runErr := execute(context.Background(), root, "test", &stderr)
	os.Stderr = realStderr
	slog.SetDefault(realLogger)
	require.NoError(t, w.Close())
	<-copied
	require.NoError(t, r.Close())

	require.NoError(t, runErr)
	out := stderr.String() + logs.String()
	assert.Contains(t, out, "msg=\"nvkectl sends no telemetry because OTEL_EXPORTER_OTLP_ENDPOINT is not a usable OTLP/gRPC endpoint; fix or unset the variable\"")
	assert.NotContains(t, out, "--otlp-endpoint")
	assert.NotContains(t, out, "secret")
	assert.Regexp(t, traceIDLineRE, out)
}

// Headers sent without TLS draw one warning that names the variable, never its value.
func TestOTLPHeadersWithoutTLSWarn(t *testing.T) {
	const token = "cleartext-token-5e6f7a8b"
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"insecure endpoint", "http://192.0.2.10:4317", true},
		{"tls endpoint", "https://192.0.2.10:4317", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer "+token)
			root := NewRootCmd("test", "test", "test", "test")
			root.AddCommand(&cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return nil }})
			root.SetArgs([]string{"probe"})
			var stderr bytes.Buffer
			root.SetErr(&stderr)

			r, w, err := os.Pipe()
			require.NoError(t, err)
			realStderr, realLogger := os.Stderr, slog.Default()
			t.Cleanup(func() { os.Stderr = realStderr; slog.SetDefault(realLogger) })
			os.Stderr = w
			var logs bytes.Buffer
			copied := make(chan struct{})
			go func() { _, _ = io.Copy(&logs, r); close(copied) }()
			runErr := execute(context.Background(), root, "test", &stderr)
			os.Stderr = realStderr
			slog.SetDefault(realLogger)
			require.NoError(t, w.Close())
			<-copied
			require.NoError(t, r.Close())

			require.NoError(t, runErr)
			out := stderr.String() + logs.String()
			assert.Equal(t, tt.want, strings.Contains(out, "OTEL_EXPORTER_OTLP_HEADERS is sent to the OTLP endpoint without TLS"))
			assert.NotContains(t, out, token)
		})
	}
}

// The collector's own rejection quotes the token, and the export warning that
// carries it to stderr has it masked, in every log mode.
func TestOTLPRejectionThatQuotesTheTokenIsMasked(t *testing.T) {
	const token = "s3cr3t-token-value"
	collector := newOTLPCollectorOn(t, "127.0.0.1", "bad Bearer "+token+"; invalid token "+token)
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + collector.addr,
		"OTEL_EXPORTER_OTLP_HEADERS":  "authorization=Bearer%20" + token,
	}
	for _, extra := range [][]string{{}, {"-v"}, {"--json-logs"}} {
		_, stderr, err := runBootstrapDryRunEnv(t, env, extra...)

		require.NoError(t, err)
		assert.NotContains(t, stderr, token, "%v", extra)
		assert.Contains(t, stderr, exportWarning, "the rejection is reported, %v", extra)
		assert.Contains(t, stderr, "<redacted>", "with the credential masked, %v", extra)
	}
}

// nilReceiverError and nilReceiverStringer read their receiver, so calling them
// on a typed nil panics; the logger prints such a value as <nil>.
type nilReceiverError struct{ msg string }

func (e *nilReceiverError) Error() string { return e.msg }

type nilReceiverStringer struct{ msg string }

func (s *nilReceiverStringer) String() string { return s.msg }

// The logger nvkectl installs masks registered credentials in the message and in
// attributes, in both log formats, including a credential the handler would
// escape.
func TestLoggerMasksCredentials(t *testing.T) {
	const plain, escaped = "logger-token-8a9b0c1d", "qu\"ot\\e\nd-tok3n-7f"
	telemetry.RegisterHeaderSecrets(map[string]string{"authorization": "Bearer " + plain, "x-token": escaped})
	realStderr, realLogger := os.Stderr, slog.Default()
	t.Cleanup(func() { os.Stderr = realStderr; slog.SetDefault(realLogger) })

	// logged returns what setupLogging's logger writes to stderr for emit.
	logged := func(t *testing.T, jsonLogs bool, emit func()) string {
		t.Helper()
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = w
		require.NoError(t, setupLogging(false, jsonLogs, nil))
		emit()
		require.NoError(t, w.Close())
		body, err := io.ReadAll(r)
		require.NoError(t, err)
		_ = r.Close()
		return string(body)
	}

	for _, jsonLogs := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", jsonLogs), func(t *testing.T) {
			body := logged(t, jsonLogs, func() {
				slog.Info("helm said "+plain, "output", "token "+plain)
			})
			assert.NotContains(t, body, plain)
			assert.Contains(t, body, "<redacted>")

			body = logged(t, jsonLogs, func() {
				slog.Info("in the message "+escaped,
					"attr", escaped,
					slog.Group("g", slog.String("inner", escaped)),
					"err", errors.New("failed: "+escaped),
					"bytes", []byte(escaped),
					"list", []string{escaped},
					"nilerr", (*nilReceiverError)(nil),
					"nilstringer", (*nilReceiverStringer)(nil),
				)
				slog.With("bound", escaped).Info("with")
			})
			assert.NotContains(t, body, "tok3n-7f", "%s", body)
			assert.GreaterOrEqual(t, strings.Count(body, "<redacted>"), 7, "%s", body)
		})
	}
}
