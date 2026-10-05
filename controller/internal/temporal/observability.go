// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/log"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/otelx"
)

const (
	operationHeader = "cwe-operation-id"
)

// OperationPropagator carries a server-generated operation ID through Temporal
// headers independently of tracing and sampling.
type OperationPropagator struct{}

func injectOperation(id string, writer workflow.HeaderWriter) error {
	if id == "" {
		return nil
	}
	payload, err := converter.GetDefaultDataConverter().ToPayload(id)
	if err != nil {
		return fmt.Errorf("encode operation ID: %w", err)
	}
	writer.Set(operationHeader, payload)
	return nil
}

func extractOperation(reader workflow.HeaderReader) (string, error) {
	var id string
	if payload, ok := reader.Get(operationHeader); ok {
		if err := converter.GetDefaultDataConverter().FromPayload(payload, &id); err != nil {
			return "", fmt.Errorf("decode operation ID: %w", err)
		}
	}
	return id, nil
}

// Inject implements workflow.ContextPropagator.
func (OperationPropagator) Inject(ctx context.Context, writer workflow.HeaderWriter) error {
	return injectOperation(otelx.OperationID(ctx), writer)
}

// Extract implements workflow.ContextPropagator.
func (OperationPropagator) Extract(ctx context.Context, reader workflow.HeaderReader) (context.Context, error) {
	id, err := extractOperation(reader)
	if err != nil || id == "" {
		return ctx, err
	}
	return context.WithValue(ctx, otelx.OperationKey{}, id), nil
}

// InjectFromWorkflow implements workflow.ContextPropagator.
func (OperationPropagator) InjectFromWorkflow(ctx workflow.Context, writer workflow.HeaderWriter) error {
	return injectOperation(otelx.OperationID(ctx), writer)
}

// ExtractToWorkflow implements workflow.ContextPropagator.
func (OperationPropagator) ExtractToWorkflow(ctx workflow.Context, reader workflow.HeaderReader) (workflow.Context, error) {
	id, err := extractOperation(reader)
	if err != nil || id == "" {
		return ctx, err
	}
	return workflow.WithValue(ctx, otelx.OperationKey{}, id), nil
}

// WorkerOptions returns the options every CWE worker runs with, so production
// and tests share one interceptor wiring.
//
// The SDK inherits the client's tracing interceptor for its worker side.
// Installing it again here creates duplicate workflow/activity spans.
func WorkerOptions() worker.Options {
	return worker.Options{Interceptors: []interceptor.WorkerInterceptor{&OutcomeInterceptor{}}}
}

// OutcomeInterceptor records workflow returns and activity-attempt outcomes.
// Register after the tracing interceptor so its loggers have the active span.
type OutcomeInterceptor struct {
	interceptor.WorkerInterceptorBase
}

// InterceptWorkflow implements interceptor.WorkerInterceptor.
func (*OutcomeInterceptor) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	return &outcomeWorkflow{WorkflowInboundInterceptorBase: interceptor.WorkflowInboundInterceptorBase{Next: next}}
}

// InterceptActivity implements interceptor.WorkerInterceptor.
func (*OutcomeInterceptor) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &outcomeActivity{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}}
}

type outcomeWorkflow struct {
	interceptor.WorkflowInboundInterceptorBase
}

func (i *outcomeWorkflow) Init(out interceptor.WorkflowOutboundInterceptor) error {
	return i.Next.Init(&operationWorkflowLogger{WorkflowOutboundInterceptorBase: interceptor.WorkflowOutboundInterceptorBase{Next: out}})
}

func (i *outcomeWorkflow) ExecuteWorkflow(ctx workflow.Context, in *interceptor.ExecuteWorkflowInput) (any, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Workflow started", "event", "workflow.started")
	start := workflow.Now(ctx)
	result, err := i.Next.ExecuteWorkflow(ctx, in)
	outcome := executionOutcome(err)
	if workflow.IsContinueAsNewError(err) {
		outcome = "continued_as_new"
	}
	fields := []any{"event", "workflow.returned", "outcome", outcome, "duration_ms", workflow.Now(ctx).Sub(start).Milliseconds()}
	logOutcome(logger, "Workflow returned", fields, err, outcome)
	return result, err
}

type operationWorkflowLogger struct {
	interceptor.WorkflowOutboundInterceptorBase
}

func (i *operationWorkflowLogger) GetLogger(ctx workflow.Context) log.Logger {
	return withOperation(i.Next.GetLogger(ctx), otelx.OperationID(ctx))
}

type outcomeActivity struct {
	interceptor.ActivityInboundInterceptorBase
}

func (i *outcomeActivity) Init(out interceptor.ActivityOutboundInterceptor) error {
	return i.Next.Init(&operationActivityLogger{ActivityOutboundInterceptorBase: interceptor.ActivityOutboundInterceptorBase{Next: out}})
}

func (i *outcomeActivity) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("Activity attempt started", "event", "activity.started")
	start := time.Now()
	result, err := i.Next.ExecuteActivity(ctx, in)
	outcome := executionOutcome(err)
	fields := []any{"event", "activity.returned", "outcome", outcome, "duration_ms", time.Since(start).Milliseconds()}
	logOutcome(logger, "Activity attempt returned", fields, err, outcome)
	return result, err
}

type operationActivityLogger struct {
	interceptor.ActivityOutboundInterceptorBase
}

func (i *operationActivityLogger) GetLogger(ctx context.Context) log.Logger {
	return withOperation(i.Next.GetLogger(ctx), otelx.OperationID(ctx))
}

func withOperation(logger log.Logger, id string) log.Logger {
	if id == "" {
		return logger
	}
	return log.With(logger, "operation_id", id)
}

func executionOutcome(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case sdktemporal.IsCanceledError(err), errors.Is(err, context.Canceled):
		return "canceled"
	case sdktemporal.IsTimeoutError(err), errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	default:
		return "failed"
	}
}

func logOutcome(logger log.Logger, message string, fields []any, err error, outcome string) {
	if err != nil && outcome != "continued_as_new" {
		logger.Error(message, append(fields, "error", err)...)
		return
	}
	logger.Info(message, fields...)
}
