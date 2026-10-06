// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// fakeWorkflowStarter records the context each workflow start receives.
type fakeWorkflowStarter struct {
	client.Client
	mu       sync.Mutex
	contexts map[string]context.Context
	errs     map[string]error
}

func (f *fakeWorkflowStarter) ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts[opts.ID] = ctx
	if err := f.errs[opts.ID]; err != nil {
		return nil, err
	}
	return fakeWorkflowRun{id: opts.ID}, nil
}

type fakeWorkflowRun struct {
	client.WorkflowRun
	id string
}

func (r fakeWorkflowRun) GetID() string    { return r.id }
func (r fakeWorkflowRun) GetRunID() string { return "run-" + r.id }

// installRecordingTracer records spans in memory and sets the trace-context
// propagator, restoring the previous globals when the test ends.
func installRecordingTracer(t *testing.T) (*tracetest.SpanRecorder, trace.Tracer) {
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
	return recorder, provider.Tracer("test")
}

// newOutboxEntry returns an outbox entry whose trace context was captured from a
// request span, as CreateWorkflowOutboxEntry does inside the API handler.
func newOutboxEntry(t *testing.T, tracer trace.Tracer, workflowID string, withTrace bool) (db.WorkflowOutbox, trace.SpanContext) {
	t.Helper()
	entry := db.WorkflowOutbox{
		ID:            uuid.New(),
		WorkflowType:  "ProvisionCluster",
		WorkflowID:    workflowID,
		WorkflowInput: json.RawMessage(`{"clusterID":"c1"}`),
	}
	if !withTrace {
		return entry, trace.SpanContext{}
	}
	ctx, span := tracer.Start(context.Background(), "request-"+workflowID)
	span.End()
	entry.TraceContext = otelx.Capture(ctx)
	return entry, span.SpanContext()
}

func runOutboxBatch(t *testing.T, entries []db.WorkflowOutbox, starter *fakeWorkflowStarter, markErr ...error) *testutil.MockTx {
	t.Helper()
	tx := &testutil.MockTx{}
	tx.On("ListUnprocessedWorkflowOutboxEntries", mock.Anything, 10).Return(entries, nil)
	tx.On("MarkWorkflowOutboxEntryProcessed", mock.Anything, mock.Anything).Return(append(markErr, nil)[0])
	database := &testutil.MockDB{MockTx: tx}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	processor := NewOutboxProcessor(config.Config{OutboxBatchSize: 10}, slog.New(slog.DiscardHandler), database, starter)
	require.NoError(t, processor.processBatch(context.Background()))
	return tx
}

func newStarter() *fakeWorkflowStarter {
	return &fakeWorkflowStarter{contexts: map[string]context.Context{}, errs: map[string]error{}}
}

// markedContext returns the context MarkWorkflowOutboxEntryProcessed received for id.
func markedContext(t *testing.T, tx *testutil.MockTx, id uuid.UUID) context.Context {
	t.Helper()
	for _, call := range tx.Calls {
		if call.Method == "MarkWorkflowOutboxEntryProcessed" && call.Arguments.Get(1) == id {
			ctx, ok := call.Arguments.Get(0).(context.Context)
			require.True(t, ok)
			return ctx
		}
	}
	require.Failf(t, "entry was not marked processed", "id %s", id)
	return nil
}

func TestOutboxEachEntryContinuesItsOwnTrace(t *testing.T) {
	recorder, tracer := installRecordingTracer(t)
	first, firstRequest := newOutboxEntry(t, tracer, "wf-1", true)
	second, secondRequest := newOutboxEntry(t, tracer, "wf-2", true)
	require.NotEqual(t, firstRequest.TraceID(), secondRequest.TraceID())
	starter := newStarter()

	tx := runOutboxBatch(t, []db.WorkflowOutbox{first, second}, starter)

	for _, tc := range []struct {
		entry   db.WorkflowOutbox
		request trace.SpanContext
	}{{first, firstRequest}, {second, secondRequest}} {
		started := trace.SpanContextFromContext(starter.contexts[tc.entry.WorkflowID])
		require.Equal(t, tc.request.TraceID(), started.TraceID(), "%s starts under its own request's trace", tc.entry.WorkflowID)
		require.Equal(t, tc.request.TraceID(), trace.SpanContextFromContext(markedContext(t, tx, tc.entry.ID)).TraceID())
	}

	dispatch := map[trace.TraceID]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		if span.Name() == "outbox.dispatch" {
			dispatch[span.SpanContext().TraceID()] = span
		}
	}
	require.Len(t, dispatch, 2)
	require.Equal(t, firstRequest.SpanID(), dispatch[firstRequest.TraceID()].Parent().SpanID(), "dispatch is a child of the request span")
	require.Equal(t, secondRequest.SpanID(), dispatch[secondRequest.TraceID()].Parent().SpanID())

	// The mark-processed write runs under the dispatch span, not the request span.
	for _, tc := range []struct {
		entry   db.WorkflowOutbox
		request trace.SpanContext
	}{{first, firstRequest}, {second, secondRequest}} {
		marked := trace.SpanContextFromContext(markedContext(t, tx, tc.entry.ID))
		require.Equal(t, dispatch[tc.request.TraceID()].SpanContext().SpanID(), marked.SpanID(), "%s is marked under its dispatch span", tc.entry.WorkflowID)
	}
}

func TestOutboxEntryWithoutTraceContextStillDispatches(t *testing.T) {
	recorder, tracer := installRecordingTracer(t)
	legacy, _ := newOutboxEntry(t, tracer, "wf-legacy", false)
	starter := newStarter()

	tx := runOutboxBatch(t, []db.WorkflowOutbox{legacy}, starter)

	require.Contains(t, starter.contexts, "wf-legacy")
	tx.AssertCalled(t, "MarkWorkflowOutboxEntryProcessed", mock.Anything, legacy.ID)
	var dispatches int
	for _, span := range recorder.Ended() {
		if span.Name() == "outbox.dispatch" {
			dispatches++
			require.False(t, span.Parent().IsValid(), "a legacy row starts a new trace")
		}
	}
	require.Equal(t, 1, dispatches)
}

func TestOutboxStartFailureIsRecordedAndLeavesEntryPending(t *testing.T) {
	recorder, tracer := installRecordingTracer(t)
	failing, _ := newOutboxEntry(t, tracer, "wf-fail", true)
	healthy, _ := newOutboxEntry(t, tracer, "wf-ok", true)
	starter := newStarter()
	starter.errs["wf-fail"] = errors.New("temporal unavailable")

	tx := runOutboxBatch(t, []db.WorkflowOutbox{failing, healthy}, starter)

	tx.AssertNotCalled(t, "MarkWorkflowOutboxEntryProcessed", mock.Anything, failing.ID)
	tx.AssertCalled(t, "MarkWorkflowOutboxEntryProcessed", mock.Anything, healthy.ID)
	for _, span := range recorder.Ended() {
		if span.Name() != "outbox.dispatch" {
			continue
		}
		wantFailed := span.Attributes()[0].Value.AsString() == "wf-fail"
		require.Equal(t, wantFailed, span.Status().Code == codes.Error, "dispatch span for %v", span.Attributes())
	}
}

func TestOutboxAlreadyStartedWorkflowIsMarkedProcessed(t *testing.T) {
	_, tracer := installRecordingTracer(t)
	entry, _ := newOutboxEntry(t, tracer, "wf-dup", true)
	starter := newStarter()
	starter.errs["wf-dup"] = &serviceerror.WorkflowExecutionAlreadyStarted{}

	tx := runOutboxBatch(t, []db.WorkflowOutbox{entry}, starter)

	tx.AssertCalled(t, "MarkWorkflowOutboxEntryProcessed", mock.Anything, entry.ID)
}

func TestOutboxMarkFailureIsRecordedOnDispatchSpan(t *testing.T) {
	recorder, tracer := installRecordingTracer(t)
	entry, _ := newOutboxEntry(t, tracer, "wf-mark", true)
	starter := newStarter()

	runOutboxBatch(t, []db.WorkflowOutbox{entry}, starter, errors.New("write failed"))

	for _, span := range recorder.Ended() {
		if span.Name() == "outbox.dispatch" {
			require.Equal(t, codes.Error, span.Status().Code)
			return
		}
	}
	require.Fail(t, "no outbox.dispatch span recorded")
}
