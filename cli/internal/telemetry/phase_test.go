// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// useJSONLogger makes the run's log handler the default logger for the test and
// returns the buffer it writes to. Tests that call it must not run in parallel.
func useJSONLogger(t *testing.T, run *Run) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(run.LogHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func spanID(ctx context.Context) string {
	return trace.SpanContextFromContext(ctx).SpanID().String()
}

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var rec map[string]any
		require.NoError(t, dec.Decode(&rec))
		records = append(records, rec)
	}
	return records
}

func TestStartPhaseCreatesAChildSpanOfTheRoot(t *testing.T) {
	run, rec := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	useJSONLogger(t, run)

	_, end := run.StartPhase(ctx, "preflight")
	end(nil)
	run.Finish(nil)

	spans := rec.Ended()
	require.Len(t, spans, 2, "the phase span and the root span")
	phase, root := spans[0], spans[1]
	assert.Equal(t, "phase.preflight", phase.Name())
	assert.Equal(t, root.SpanContext().SpanID(), phase.Parent().SpanID())
	assert.Equal(t, root.SpanContext().TraceID(), phase.SpanContext().TraceID())
	assert.Contains(t, phase.Attributes(), attribute.String("nvke.phase", "preflight"))
	assert.Contains(t, phase.Attributes(), attribute.String("nvke.outcome", OutcomeSucceeded))
	assert.Equal(t, codes.Unset, phase.Status().Code)
}

func TestStartPhaseRecordsAFailure(t *testing.T) {
	run, rec := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	buf := useJSONLogger(t, run)

	_, end := run.StartPhase(ctx, "engine_deploy")
	end(errors.New("helm failed"))

	phase := rec.Ended()[0]
	assert.Equal(t, codes.Error, phase.Status().Code)
	assert.Equal(t, "helm failed", phase.Status().Description)
	assert.Contains(t, phase.Attributes(), attribute.String("nvke.outcome", OutcomeFailed))
	records := decodeRecords(t, buf)
	require.Len(t, records, 1)
	assert.Equal(t, OutcomeFailed, records[0]["outcome"])
}

func TestStartPhaseLogsOnePhaseFinishedRecord(t *testing.T) {
	run, _ := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	buf := useJSONLogger(t, run)

	phaseCtx, end := run.StartPhase(ctx, "site_cluster")
	end(nil)
	end(errors.New("ignored: the phase already ended"))

	records := decodeRecords(t, buf)
	require.Len(t, records, 1, "ending twice logs once")
	rec := records[0]
	assert.Equal(t, "phase finished", rec["msg"])
	assert.Equal(t, "site_cluster", rec["phase"])
	assert.Equal(t, OutcomeSucceeded, rec["outcome"])
	assert.GreaterOrEqual(t, rec["duration_ms"], float64(0))
	assert.Equal(t, run.TraceID(), rec["trace_id"])
	assert.Equal(t, spanID(phaseCtx), rec["span_id"], "the record carries the phase span's ID")
}

func TestLogHandlerUsesThePhaseForLoggersWithoutAContext(t *testing.T) {
	run, _ := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	var buf bytes.Buffer
	// Built before the phase starts, as a component's logger is.
	logger := slog.New(run.LogHandler(slog.NewJSONHandler(&buf, nil))).With("component", "controllerdeploy")

	logger.Info("before the phase")
	phaseCtx, end := run.StartPhase(ctx, "engine_deploy")
	logger.Info("inside the phase")
	end(nil)
	logger.Info("after the phase")

	records := decodeRecords(t, &buf)
	var before, inside, after map[string]any
	for _, rec := range records {
		switch rec["msg"] {
		case "before the phase":
			before = rec
		case "inside the phase":
			inside = rec
		case "after the phase":
			after = rec
		}
	}
	rootSpan := run.SpanContext().SpanID().String()
	assert.Equal(t, rootSpan, before["span_id"])
	assert.NotContains(t, before, "phase")
	assert.Equal(t, spanID(phaseCtx), inside["span_id"])
	assert.Equal(t, "engine_deploy", inside["phase"])
	assert.Equal(t, "controllerdeploy", inside["component"])
	assert.Equal(t, rootSpan, after["span_id"], "the root span again once the phase ended")
	assert.NotContains(t, after, "phase")
}

func TestLogHandlerAddsOperationAndClusterToLoggersBuiltEarlier(t *testing.T) {
	run, _ := newTestRun(t)
	run.Start(context.Background(), "nvkectl bootstrap site")
	var buf bytes.Buffer
	logger := slog.New(run.LogHandler(slog.NewJSONHandler(&buf, nil)))

	logger.Info("before the fields are set")
	run.SetLogFields("bootstrap_site", "csc-01")
	logger.Info("after")
	logger.Info("with its own cluster", "cluster", "other")

	raw := buf.String()
	records := decodeRecords(t, &buf)
	require.Len(t, records, 3)
	assert.NotContains(t, records[0], "operation")
	assert.NotContains(t, records[0], "cluster")
	assert.Equal(t, "bootstrap_site", records[1]["operation"])
	assert.Equal(t, "csc-01", records[1]["cluster"])
	assert.Equal(t, "other", records[2]["cluster"], "a record's own field wins")
	assert.Equal(t, 2, strings.Count(raw, `"cluster"`), "one in the second record, one in the third: no duplicates")
}

func TestStartPhaseOnARunThatHasNotStarted(t *testing.T) {
	for name, run := range map[string]*Run{"nil run": nil, "run without Start": NewRun("v1")} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			ctx, end := run.StartPhase(context.Background(), "preflight")
			end(nil)

			assert.NotNil(t, ctx)
			records := decodeRecords(t, &buf)
			require.Len(t, records, 1, "the duration is logged with no telemetry")
			assert.Equal(t, "preflight", records[0]["phase"])
			assert.NotPanics(t, func() { run.SetLogFields("op", "cluster") })
		})
	}
}

// TestPhaseFailureExportsItsErrorMasked checks that a credential in the error
// that failed a phase reaches neither the phase's log record nor the exported
// span.
func TestPhaseFailureExportsItsErrorMasked(t *testing.T) {
	const token = "phase-token-1c2d3e4f"
	RegisterHeaderSecrets(map[string]string{"authorization": "Bearer " + token})
	collector := newFakeCollector(t)
	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	buf := useJSONLogger(t, run)

	_, end := run.StartPhase(ctx, "engine_deploy")
	end(errors.New("helm failed with " + token))
	run.Finish(nil)

	assert.NotContains(t, buf.String(), token, "the log record has no error text")
	var found bool
	for _, span := range collector.receivedSpans() {
		if span.GetName() != "phase.engine_deploy" {
			continue
		}
		found = true
		assert.Equal(t, "helm failed with <redacted>", span.GetStatus().GetMessage())
		for _, event := range span.GetEvents() {
			for _, kv := range event.GetAttributes() {
				assert.NotContains(t, kv.GetValue().GetStringValue(), token, kv.GetKey())
			}
		}
	}
	assert.True(t, found, "the phase span reached the collector")
}

// TestOTLPExportsPhaseSpanAndFieldsForLoggersWithoutAContext checks that, with
// OTLP on, a record logged through a context-free logger during a phase reaches
// stderr and the collector with the phase span's ID and the same fields.
func TestOTLPExportsPhaseSpanAndFieldsForLoggersWithoutAContext(t *testing.T) {
	collector := newFakeCollector(t)
	var stderr bytes.Buffer
	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	logger := slog.New(run.LogHandler(slog.NewJSONHandler(&stderr, nil))).With("component", "controllerdeploy")
	run.SetLogFields("bootstrap_site", "csc-01")

	phaseCtx, end := run.StartPhase(ctx, "engine_deploy")
	logger.Info("inside the phase")
	end(nil)
	run.Finish(nil)

	var local map[string]any
	for _, rec := range decodeRecords(t, &stderr) {
		if rec["msg"] == "inside the phase" {
			local = rec
		}
	}
	require.NotNil(t, local)
	assert.Equal(t, spanID(phaseCtx), local["span_id"])

	var exported *logspb.LogRecord
	for _, rec := range collector.received() {
		if rec.GetBody().GetStringValue() == "inside the phase" {
			exported = rec
		}
	}
	require.NotNil(t, exported, "the record reached the collector")
	assert.Equal(t, spanID(phaseCtx), hex.EncodeToString(exported.GetSpanId()))
	attrs := attributes(exported)
	assert.Equal(t, "engine_deploy", attrs["phase"])
	assert.Equal(t, "bootstrap_site", attrs["operation"])
	assert.Equal(t, "csc-01", attrs["cluster"])
}

// TestLogHandlerPutsTraceFieldsBeforeRunFields pins the order of the fields on
// a text line: the e2e gitleaks allowlist matches the line whole, in this order.
func TestLogHandlerPutsTraceFieldsBeforeRunFields(t *testing.T) {
	run, _ := newTestRun(t)
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	var buf bytes.Buffer
	logger := slog.New(run.LogHandler(slog.NewTextHandler(&buf, nil)))
	run.SetLogFields("bootstrap_site", "csc-01")

	phaseCtx, end := run.StartPhase(ctx, "preflight")
	logger.Info("inside", "k", "v")
	end(nil)

	line := strings.Split(buf.String(), "\n")[0]
	assert.Contains(t, line, `msg=inside k=v trace_id=`+run.TraceID()+` span_id=`+spanID(phaseCtx)+` operation=bootstrap_site cluster=csc-01 phase=preflight`)
}

// TestOTLPLoggerWithAttrsAndGroupKeepsTheRunFields checks that a logger made
// with With and WithGroup from the OTLP handler still gets the phase and the
// run's fields, so its handler is still the one that adds them.
func TestOTLPLoggerWithAttrsAndGroupKeepsTheRunFields(t *testing.T) {
	collector := newFakeCollector(t)
	var stderr bytes.Buffer
	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	run.SetLogFields("bootstrap_site", "csc-01")
	logger := slog.New(run.LogHandler(slog.NewJSONHandler(&stderr, nil)))

	phaseCtx, end := run.StartPhase(ctx, "preflight")
	logger.With("component", "x").WithGroup("g").Info("grouped", "k", "v")
	end(nil)
	run.Finish(nil)

	var local map[string]any
	for _, rec := range decodeRecords(t, &stderr) {
		if rec["msg"] == "grouped" {
			local = rec
		}
	}
	require.NotNil(t, local)
	assert.Equal(t, "x", local["component"])
	group, ok := local["g"].(map[string]any)
	require.True(t, ok, "the record's attributes are in the group")
	assert.Equal(t, spanID(phaseCtx), group["span_id"])
	assert.Equal(t, "preflight", group["phase"])
	assert.Equal(t, "csc-01", group["cluster"])
	assert.Equal(t, "v", group["k"])
}
