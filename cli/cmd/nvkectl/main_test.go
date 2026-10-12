// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

func TestReportFailure(t *testing.T) {
	t.Parallel()

	recovery := &recoveryError{fmt.Errorf("deploy: %w; bootstrap cluster %q retained", context.Canceled, "kind-x")}

	tests := []struct {
		name       string
		err        error
		canceled   bool
		sig        syscall.Signal
		wantCode   int
		wantOutput []string
		denyOutput []string
	}{
		{
			name: "plain failure", err: errors.New("boom"),
			wantCode: exitError, wantOutput: []string{"Error: boom"},
		},
		{
			name: "SIGINT is one line", err: fmt.Errorf("get: %w", context.Canceled), canceled: true, sig: syscall.SIGINT,
			wantCode: exitSIGINT, wantOutput: []string{"nvkectl: canceled"}, denyOutput: []string{"Error:"},
		},
		{
			name: "SIGTERM", err: context.Canceled, canceled: true, sig: syscall.SIGTERM,
			wantCode: exitSIGTERM, wantOutput: []string{"nvkectl: canceled"},
		},
		{
			name: "trace ID survives a cancellation", err: &tracedError{err: fmt.Errorf("get: %w", context.Canceled), traceID: "4bf92f3577b34da6a3ce929d0e0e4736"},
			canceled: true, sig: syscall.SIGINT,
			wantCode: exitSIGINT, wantOutput: []string{"nvkectl: canceled", "Trace ID: 4bf92f3577b34da6a3ce929d0e0e4736"}, denyOutput: []string{"Error:"},
		},
		{
			name: "recovery advice survives a cancellation", err: fmt.Errorf("bootstrap: %w", recovery), canceled: true, sig: syscall.SIGINT,
			wantCode: exitSIGINT, wantOutput: []string{"nvkectl: canceled", `bootstrap cluster "kind-x" retained`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			if got := reportFailure(&out, tc.err, tc.canceled, tc.sig); got != tc.wantCode {
				t.Errorf("exit code = %d, want %d", got, tc.wantCode)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output = %q, want it to contain %q", out.String(), want)
				}
			}
			for _, deny := range tc.denyOutput {
				if strings.Contains(out.String(), deny) {
					t.Errorf("output = %q, want it not to contain %q", out.String(), deny)
				}
			}
		})
	}
}
