//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tracing checks that a request's trace context survives the database
// outbox and shows up in the Temporal workflow, its activities, and the logs.
package tracing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.temporal.io/sdk/activity"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/e2e/framework"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/db/migrations"
	"github.com/dsx-ai-factory/nke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nke/controller/internal/server"
	cwetemporal "github.com/dsx-ai-factory/nke/controller/internal/temporal"
)

const requestSpanName = "POST /api/clusters"

// TestTraceContinuesThroughOutbox sends a real POST /api/clusters through the
// CWE handler, PostgreSQL, the outbox processor, and a Temporal dev server.
// Only provisioning is replaced: a stand-in ProvisionCluster workflow reads the
// new cluster back through the real DB layer and injects a controlled outcome,
// so no infrastructure is created.
func TestTraceContinuesThroughOutbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	temporalServer := framework.StartTemporalDevServer(t, ctx)
	cfg := config.Config{
		DBConnectionString:      newDatabase(ctx, t, framework.StartPostgres(t)),
		DBMaxOpenConnections:    5,
		DBQueryLogLevel:         "info",
		TemporalHostPort:        temporalServer.FrontendHostPort(),
		TemporalNamespace:       "default",
		TemporalTaskQueue:       framework.TaskQueueName("tracing-e2e", t.Name()),
		OutboxPollingInterval:   100 * time.Millisecond,
		OutboxBatchSize:         100,
		SupportedInfraProviders: []string{"infra"},
		ProvisionControlPlaneWithControlPlaneController: true,
		// Enabled turns on the HTTP and Temporal tracing instrumentation. No
		// endpoint is set, so nothing is exported; spans are recorded in memory.
		Tracing: config.TracingConfig{Enabled: true},
	}

	spans := installTracing(t)
	logs := &logBuffer{}
	logger := slog.New(otelx.LogHandler{Handler: slog.NewJSONHandler(logs, &slog.HandlerOptions{ReplaceAttr: otelx.NormalizeLogAttr})})

	bunDB, err := db.NewBunDB(cfg, logger)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bunDB.Close()) })
	require.NoError(t, migrations.Migrate(ctx, bunDB))
	database := db.NewDB(bunDB)

	temporalClient, err := cwetemporal.NewClient(cfg, logger)
	require.NoError(t, err)
	t.Cleanup(temporalClient.Close)

	w := worker.New(temporalClient, cfg.TemporalTaskQueue, cwetemporal.WorkerOptions())
	w.RegisterWorkflowWithOptions(standInProvision, workflow.RegisterOptions{Name: "ProvisionCluster"})
	w.RegisterActivityWithOptions(func(ctx context.Context, clusterID string) error {
		cluster, err := database.GetCluster(ctx, uuid.MustParse(clusterID))
		if err != nil {
			return err
		}
		activity.GetLogger(ctx).Info("cluster read", "cluster_id", clusterID)
		if strings.HasPrefix(cluster.Name, "fail-") {
			return sdktemporal.NewNonRetryableApplicationError("injected failure", "InjectedFailure", nil)
		}
		if strings.HasPrefix(cluster.Name, "retry-") && activity.GetInfo(ctx).Attempt == 1 {
			return fmt.Errorf("injected transient failure")
		}
		return nil
	}, activity.RegisterOptions{Name: "ReadCluster"})
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	processor := cwetemporal.NewOutboxProcessor(cfg, logger, database, temporalClient)
	processor.Start(ctx)
	t.Cleanup(processor.Stop)

	srv, err := server.NewServer(cfg, logger, database, temporalClient, nil, nil)
	require.NoError(t, err)
	httpServer := httptest.NewServer(srv.Handler)
	t.Cleanup(httpServer.Close)

	for _, scenario := range []struct {
		name        string
		traceparent string
		retries     bool
		wantErr     string
	}{
		{name: "success", traceparent: "00-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-2222222222222222-01"},
		{name: "retry", traceparent: "invalid-traceparent", retries: true},
		{name: "fail", wantErr: "injected failure"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			responseBody, header := createCluster(ctx, t, httpServer.URL, scenario.name, scenario.traceparent)
			traceID := header.Get("X-Trace-ID")
			require.Len(t, traceID, 32)
			if scenario.name == "success" {
				require.Equal(t, strings.Split(scenario.traceparent, "-")[1], traceID, "a valid incoming traceparent is continued")
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
			require.Contains(t, entry.TraceContext["traceparent"], traceID, "the outbox row stores the request's trace context")

			err := temporalClient.GetWorkflow(ctx, workflowID, "").Get(ctx, nil)
			if scenario.wantErr != "" {
				require.ErrorContains(t, err, scenario.wantErr)
			} else {
				require.NoError(t, err)
			}

			assertTrace(t, spans.Ended(), traceID, scenario.retries)
			assertLogs(t, logs.snapshot(), traceID, scenario.name)
		})
	}
}

// installTracing records spans in memory and sets the trace-context propagator,
// as otelx.Init does for a real exporter, then restores the previous globals.
func installTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return recorder
}

// newDatabase creates a database of its own on the PostgreSQL server at baseDSN
// and returns its DSN. CI hands every e2e suite the same server, and the
// worker-binary suites run controllers that act on every cluster row they see.
func newDatabase(ctx context.Context, t *testing.T, baseDSN string) string {
	t.Helper()
	name := "tracing_e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(baseDSN)))
	_, err := admin.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(ctx), "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	dsn, err := url.Parse(baseDSN)
	require.NoError(t, err)
	dsn.Path = "/" + name
	return dsn.String()
}

func createCluster(ctx context.Context, t *testing.T, baseURL, name, traceparent string) ([]byte, http.Header) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"etcdEncrypted":                 false,
		"name":                          name + "-" + uuid.NewString()[:8],
		"cniPlugin":                     "calico",
		"controlPlaneKubernetesVersion": "1.34.1",
		"infra":                         map[string]any{"cluster": manifest("DockerCluster")},
		"controlPlaneConfig":            map[string]any{"desiredCount": 1, "infra": map[string]any{"machineTemplate": manifest("DockerMachineTemplate")}},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/clusters", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", traceparent)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode, string(body))
	return body, response.Header
}

func manifest(kind string) map[string]any {
	return map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       kind,
		"metadata":   map[string]any{"name": "tracing-e2e"},
		"spec":       map[string]any{},
	}
}

func standInProvision(ctx workflow.Context, input map[string]any) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy:         &sdktemporal.RetryPolicy{InitialInterval: 100 * time.Millisecond, MaximumAttempts: 2},
	})
	return workflow.ExecuteActivity(ctx, "ReadCluster", input["clusterID"]).Get(ctx, nil)
}

// assertTrace checks that one request produced a single connected trace, from
// the HTTP span through the outbox dispatch to the workflow and its activities.
func assertTrace(t *testing.T, ended []sdktrace.ReadOnlySpan, traceID string, retried bool) {
	t.Helper()
	counts := map[string]int{}
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range ended {
		if span.SpanContext().TraceID().String() == traceID {
			counts[span.Name()]++
			byName[span.Name()] = span
		}
	}
	for _, name := range []string{requestSpanName, "outbox.dispatch", "StartWorkflow:ProvisionCluster", "RunWorkflow:ProvisionCluster"} {
		require.Equal(t, 1, counts[name], "missing or duplicate span %s: %v", name, counts)
	}
	for child, parent := range map[string]string{
		"outbox.dispatch":                requestSpanName,
		"StartWorkflow:ProvisionCluster": "outbox.dispatch",
		"RunWorkflow:ProvisionCluster":   "StartWorkflow:ProvisionCluster",
		"StartActivity:ReadCluster":      "RunWorkflow:ProvisionCluster",
		"RunActivity:ReadCluster":        "StartActivity:ReadCluster",
	} {
		require.NotNil(t, byName[child], child)
		require.NotNil(t, byName[parent], parent)
		require.Equal(t, byName[parent].SpanContext().SpanID(), byName[child].Parent().SpanID(), "%s is a child of %s", child, parent)
	}
	attempts := 1
	if retried {
		attempts = 2
	}
	require.Equal(t, attempts, counts["RunActivity:ReadCluster"], "%v", counts)
	var sqlSpans int
	for name, count := range counts {
		if strings.HasPrefix(name, "SELECT") || strings.HasPrefix(name, "INSERT") {
			sqlSpans += count
		}
	}
	require.Positive(t, sqlSpans, "no SQL spans in the trace: %v", counts)
}

// assertLogs checks the structured logs written under the request's trace ID:
// one HTTP completion, SQL lines, and the workflow and activity outcomes.
func assertLogs(t *testing.T, data []byte, traceID, scenario string) {
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
		// LogHandler adds trace_id and span_id, and NormalizeLogAttr renames the Temporal
		// SDK's own TraceID and SpanID fields to the same keys; JSON parsers disagree on
		// duplicate keys, so a line must never carry both.
		for _, key := range []string{"trace_id", "span_id"} {
			require.Equal(t, 1, bytes.Count(line, []byte(`"`+key+`":`)), "duplicate %s in %s", key, line)
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
	require.Equal(t, 1, events["http"], "%v", events)
	require.Positive(t, events["sql"], "%v", events)
	outcome := "succeeded"
	if scenario == "fail" {
		outcome = "failed"
	}
	require.Equal(t, 1, events["workflow.returned:"+outcome], "%v", events)
	if scenario == "retry" {
		require.Equal(t, 1, events["activity.returned:failed"], "%v", events)
		require.Equal(t, 1, events["activity.returned:succeeded"], "%v", events)
	}
}

// logBuffer is a goroutine-safe buffer: the API, outbox processor, and worker
// all log concurrently.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}
