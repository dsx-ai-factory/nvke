//go:build tracing_poc

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.temporal.io/sdk/activity"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/db/migrations"
	"github.com/dsx-ai-factory/nke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nke/controller/internal/server"
	cwetemporal "github.com/dsx-ai-factory/nke/controller/internal/temporal"
)

// This test uses the real CWE POST /api/clusters handler, PostgreSQL migrations,
// outbox processor, and Temporal service. Only provisioning is replaced by a
// harmless workflow that reads the new cluster through the real Bun DB layer.
func TestTracingPOC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := config.Config{
		DBConnectionString:      "postgres://postgres@127.0.0.1:15432/cwe_tracing_poc?sslmode=disable",
		DBMaxOpenConnections:    5,
		DBQueryLogLevel:         "info",
		TemporalHostPort:        "127.0.0.1:17233",
		TemporalNamespace:       "default",
		TemporalTaskQueue:       "tracing-poc-" + uuid.NewString(),
		OutboxPollingInterval:   100 * time.Millisecond,
		OutboxBatchSize:         100,
		SupportedInfraProviders: []string{"infra"},
		ProvisionControlPlaneWithControlPlaneController: true,
		Tracing: config.TracingConfig{Enabled: true, Endpoint: "127.0.0.1:14317", Insecure: true, ServiceName: "cwe-tracing-poc"},
	}
	logPath := os.Getenv("POC_LOG_PATH")
	if logPath == "" {
		logPath = "tracing-poc.jsonl"
	}
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, logFile.Close()) }()
	logs := &pocLogBuffer{}
	logger := slog.New(otelx.LogHandler{Handler: slog.NewJSONHandler(io.MultiWriter(logFile, logs), &slog.HandlerOptions{ReplaceAttr: otelx.NormalizeLogAttr})})
	shutdown, err := otelx.Init(ctx, cfg.Tracing, logger)
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(context.Background())) }()
	provider := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	spans := &pocSpanExporter{}
	provider.RegisterSpanProcessor(sdktrace.NewSimpleSpanProcessor(spans))
	bunDB, err := db.NewBunDB(cfg, logger)
	require.NoError(t, err)
	defer func() { require.NoError(t, bunDB.Close()) }()
	require.NoError(t, migrations.Migrate(ctx, bunDB))
	database := db.NewDB(bunDB)
	temporalClient, err := cwetemporal.NewClient(cfg, logger)
	require.NoError(t, err)
	defer temporalClient.Close()
	w := worker.New(temporalClient, cfg.TemporalTaskQueue, cwetemporal.WorkerOptions())
	w.RegisterWorkflowWithOptions(pocProvision, workflow.RegisterOptions{Name: "ProvisionCluster"})
	w.RegisterActivityWithOptions(func(ctx context.Context, clusterID string) error {
		cluster, err := database.GetCluster(ctx, uuid.MustParse(clusterID))
		if err != nil {
			return err
		}
		activity.GetLogger(ctx).Info("POC cluster read", "cluster_id", clusterID)
		if strings.HasPrefix(cluster.Name, "fail-") {
			return sdktemporal.NewNonRetryableApplicationError("injected POC failure", "POCFailure", nil)
		}
		if strings.HasPrefix(cluster.Name, "retry-") && activity.GetInfo(ctx).Attempt == 1 {
			return fmt.Errorf("injected transient POC failure")
		}
		return nil
	}, activity.RegisterOptions{Name: "POCReadCluster"})
	require.NoError(t, w.Start())
	defer w.Stop()
	processor := cwetemporal.NewOutboxProcessor(cfg, logger, database, temporalClient)
	processor.Start(ctx)
	defer processor.Stop()
	srv, err := server.NewServer(cfg, logger, database, temporalClient, nil)
	require.NoError(t, err)
	httpServer := httptest.NewServer(srv.Handler)
	defer httpServer.Close()

	for _, scenario := range []struct{ name, parent string }{
		{"success", "00-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-2222222222222222-01"},
		{"retry", "invalid-traceparent"},
		{"fail", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			body := map[string]any{
				"etcdEncrypted": false,
				"name":          scenario.name + "-" + uuid.NewString()[:8],
				"cniPlugin":     "calico", "controlPlaneKubernetesVersion": "1.34.1",
				"infra":              map[string]any{"cluster": pocManifest("DockerCluster")},
				"controlPlaneConfig": map[string]any{"desiredCount": 1, "infra": map[string]any{"machineTemplate": pocManifest("DockerMachineTemplate")}},
			}
			payload, err := json.Marshal(body)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/api/clusters", bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("traceparent", scenario.parent)
			response, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			responseBody, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, response.StatusCode, string(responseBody))
			traceID := response.Header.Get("X-Trace-ID")
			require.Len(t, traceID, 32)
			if scenario.name == "success" {
				require.Equal(t, strings.Split(scenario.parent, "-")[1], traceID)
			}
			var cluster struct {
				ID uuid.UUID `json:"id"`
			}
			require.NoError(t, json.Unmarshal(responseBody, &cluster))
			workflowID := "provision-cluster-" + cluster.ID.String()
			var entry db.WorkflowOutbox
			require.Eventually(t, func() bool {
				return bunDB.NewSelect().Model(&entry).Where("workflow_id = ?", workflowID).Scan(ctx) == nil && entry.ProcessedAt != nil
			}, 15*time.Second, 100*time.Millisecond)
			require.Contains(t, entry.TraceContext["traceparent"], traceID)
			err = temporalClient.GetWorkflow(ctx, workflowID, "").Get(ctx, nil)
			if scenario.name == "fail" {
				require.ErrorContains(t, err, "injected POC failure")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, provider.ForceFlush(ctx))
			assertPOCTrace(t, spans.Ended(), traceID, scenario.name == "retry")
			assertPOCLogs(t, logs.snapshot(), traceID, scenario.name)
			t.Logf("%s: workflow_id=%s trace=http://localhost:16686/trace/%s logs=%s", scenario.name, workflowID, traceID, logPath)
		})
	}
}

func pocManifest(kind string) map[string]any {
	return map[string]any{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": kind, "metadata": map[string]any{"name": "tracing-poc"}, "spec": map[string]any{}}
}

func pocProvision(ctx workflow.Context, input map[string]any) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy:         &sdktemporal.RetryPolicy{InitialInterval: 100 * time.Millisecond, MaximumAttempts: 2},
	})
	return workflow.ExecuteActivity(ctx, "POCReadCluster", input["clusterID"]).Get(ctx, nil)
}

func assertPOCTrace(t *testing.T, spans []sdktrace.ReadOnlySpan, traceID string, retry bool) {
	t.Helper()
	names := map[string]int{}
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range spans {
		if span.SpanContext().TraceID().String() == traceID {
			names[span.Name()]++
			byName[span.Name()] = span
		}
	}
	for _, name := range []string{"POST /api/clusters", "outbox.dispatch", "StartWorkflow:ProvisionCluster", "RunWorkflow:ProvisionCluster"} {
		require.Equal(t, 1, names[name], "missing/duplicate span %s: %v", name, names)
	}
	for child, parent := range map[string]string{
		"outbox.dispatch":                "POST /api/clusters",
		"StartWorkflow:ProvisionCluster": "outbox.dispatch",
		"RunWorkflow:ProvisionCluster":   "StartWorkflow:ProvisionCluster",
		"StartActivity:POCReadCluster":   "RunWorkflow:ProvisionCluster",
		"RunActivity:POCReadCluster":     "StartActivity:POCReadCluster",
	} {
		require.NotNil(t, byName[child], child)
		require.NotNil(t, byName[parent], parent)
		require.Equal(t, byName[parent].SpanContext().SpanID(), byName[child].Parent().SpanID(), child)
	}
	attempts := 1
	if retry {
		attempts = 2
	}
	require.Equal(t, attempts, names["RunActivity:POCReadCluster"], "%v", names)
	var sqlSpans int
	for name, count := range names {
		if strings.HasPrefix(name, "SELECT") || strings.HasPrefix(name, "INSERT") {
			sqlSpans += count
		}
	}
	require.Positive(t, sqlSpans, "no SQL spans in trace: %v", names)
}

func assertPOCLogs(t *testing.T, data []byte, traceID, scenario string) {
	t.Helper()
	events := map[string]int{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		if record["trace_id"] != traceID {
			continue
		}
		if event, ok := record["event"].(string); ok {
			events[event+":"+fmt.Sprint(record["outcome"])]++
		}
		if record["operation"] == "SELECT" || record["operation"] == "INSERT" {
			events["sql"]++
		}
		if record["msg"] == "Request completed" {
			events["http"]++
		}
	}
	require.Equal(t, 1, events["http"])
	require.Positive(t, events["sql"])
	outcome := "succeeded"
	if scenario == "fail" {
		outcome = "failed"
	}
	require.Equal(t, 1, events["workflow.returned:"+outcome], "%v", events)
	if scenario == "retry" {
		require.Equal(t, 1, events["activity.returned:failed"])
		require.Equal(t, 1, events["activity.returned:succeeded"])
	}
}

type pocLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *pocLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *pocLogBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.Buffer.Bytes())
}

// pocSpanExporter observes the SDK's real completed spans without an additional
// test-only dependency.
type pocSpanExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *pocSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (*pocSpanExporter) Shutdown(context.Context) error { return nil }

func (e *pocSpanExporter) Ended() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}
