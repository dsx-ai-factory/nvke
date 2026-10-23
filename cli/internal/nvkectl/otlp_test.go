// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
)

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
