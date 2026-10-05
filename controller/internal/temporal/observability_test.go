// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	sdktemporal "go.temporal.io/sdk/temporal"
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
