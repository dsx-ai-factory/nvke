// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/log"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

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

type outcomeActivity struct {
	interceptor.ActivityInboundInterceptorBase
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
	switch {
	case err == nil || outcome == "continued_as_new":
		logger.Info(message, fields...)
	case outcome == "canceled":
		// A worker shutdown or a user canceling is not a failure to alert on.
		logger.Warn(message, append(fields, "error", err)...)
	default:
		logger.Error(message, append(fields, "error", err)...)
	}
}
