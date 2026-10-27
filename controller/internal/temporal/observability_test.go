// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestExecutionOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "succeeded"},
		{"plain error", errors.New("boom"), "failed"},
		{"application error", sdktemporal.NewApplicationError("boom", "X"), "failed"},
		{"temporal canceled error", sdktemporal.NewCanceledError(), "canceled"},
		{"temporal timeout error", sdktemporal.NewTimeoutError(0, nil), "timed_out"},
		{"raw context canceled", context.Canceled, "canceled"},
		{"wrapped context canceled", fmt.Errorf("read cluster: %w", context.Canceled), "canceled"},
		{"raw context deadline", context.DeadlineExceeded, "timed_out"},
		{"wrapped context deadline", fmt.Errorf("read cluster: %w", context.DeadlineExceeded), "timed_out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, executionOutcome(tt.err))
		})
	}
}

func TestWorkerOptionsInstallOutcomeInterceptor(t *testing.T) {
	opts := WorkerOptions()
	require.Len(t, opts.Interceptors, 1)
	require.IsType(t, &OutcomeInterceptor{}, opts.Interceptors[0])
}

// outcomeLogs runs the workflow in Temporal's test environment with the
// production worker options and returns the "event:outcome" of every
// workflow.returned and activity.returned log line, in order.
func outcomeLogs(t *testing.T, wf func(workflow.Context) error, act func(context.Context) error) ([]string, error) {
	t.Helper()
	var buf bytes.Buffer
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(log.NewStructuredLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	env := suite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(WorkerOptions())
	env.RegisterWorkflowWithOptions(wf, workflow.RegisterOptions{Name: "Outcome"})
	env.RegisterActivityWithOptions(act, activity.RegisterOptions{Name: "Step"})
	env.ExecuteWorkflow("Outcome")

	var returned []string
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var record map[string]any
		if len(line) == 0 || json.Unmarshal(line, &record) != nil {
			continue
		}
		if event, _ := record["event"].(string); event == "workflow.returned" || event == "activity.returned" {
			returned = append(returned, fmt.Sprintf("%s:%v", event, record["outcome"]))
		}
	}
	return returned, env.GetWorkflowError()
}

func runStep(maxAttempts int32) func(workflow.Context) error {
	return func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute,
			RetryPolicy:         &sdktemporal.RetryPolicy{InitialInterval: time.Millisecond, MaximumAttempts: maxAttempts},
		})
		return workflow.ExecuteActivity(ctx, "Step").Get(ctx, nil)
	}
}

func TestOutcomeInterceptorLogsWorkflowAndActivityOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		wf      func(workflow.Context) error
		act     func(context.Context) error
		want    []string
		wantErr bool
	}{
		{
			name: "success",
			wf:   runStep(1),
			act:  func(context.Context) error { return nil },
			want: []string{"activity.returned:succeeded", "workflow.returned:succeeded"},
		},
		{
			name: "a retried activity logs each attempt and the workflow still succeeds",
			wf:   runStep(2),
			act: func(ctx context.Context) error {
				if activity.GetInfo(ctx).Attempt == 1 {
					return errors.New("transient")
				}
				return nil
			},
			want: []string{"activity.returned:failed", "activity.returned:succeeded", "workflow.returned:succeeded"},
		},
		{
			name:    "a permanent activity failure fails the workflow",
			wf:      runStep(1),
			act:     func(context.Context) error { return sdktemporal.NewNonRetryableApplicationError("boom", "Boom", nil) },
			want:    []string{"activity.returned:failed", "workflow.returned:failed"},
			wantErr: true,
		},
		{
			name: "continue as new is not a failure",
			wf: func(ctx workflow.Context) error {
				return workflow.NewContinueAsNewError(ctx, "Outcome")
			},
			act:     func(context.Context) error { return nil },
			want:    []string{"workflow.returned:continued_as_new"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outcomeLogs(t, tt.wf, tt.act)
			require.Equal(t, tt.want, got)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLogOutcomeLevel(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		outcome string
		want    string
	}{
		{"success", nil, "succeeded", "INFO"},
		{"continue as new", errors.New("continue as new"), "continued_as_new", "INFO"},
		{"canceled is not an error-level event", context.Canceled, "canceled", "WARN"},
		{"timed out", context.DeadlineExceeded, "timed_out", "ERROR"},
		{"failed", errors.New("boom"), "failed", "ERROR"},
		{
			"benign activity failures remain visible in the outcome log",
			sdktemporal.NewApplicationErrorWithOptions(
				"not ready",
				"NotReady",
				sdktemporal.ApplicationErrorOptions{Category: sdktemporal.ApplicationErrorCategoryBenign},
			),
			"failed",
			"ERROR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.NewStructuredLogger(slog.New(slog.NewJSONHandler(&buf, nil)))

			logOutcome(logger, "returned", []any{"outcome", tt.outcome}, tt.err, tt.outcome)

			var record map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
			require.Equal(t, tt.want, record["level"])
			if tt.err != nil && tt.outcome != "continued_as_new" {
				require.Contains(t, record, "error")
			}
		})
	}
}
