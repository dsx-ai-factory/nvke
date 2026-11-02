// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

func TestReportFailure(t *testing.T) {
	t.Parallel()

	// Registered credentials, such as an OTLP header value, are masked in the report.
	const reportToken, canceledToken = "report-token-1b2c3d4e", "canceled-token-3c4d5e6f"
	telemetry.RegisterHeaderSecrets(map[string]string{"authorization": "Bearer " + reportToken, "x-api-key": canceledToken})

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
		{
			name: "a credential is masked", err: errors.New("helm failed: " + reportToken),
			wantCode: exitError, wantOutput: []string{"Error: helm failed: <redacted>"}, denyOutput: []string{reportToken},
		},
		{
			name: "a credential is masked in recovery advice", err: &recoveryError{err: errors.New("keep this: " + canceledToken)}, canceled: true, sig: syscall.SIGINT,
			wantCode: exitSIGINT, wantOutput: []string{"keep this: <redacted>"}, denyOutput: []string{canceledToken},
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

// TestRunReturnsInsteadOfExiting is the guard on what makes this package safe
// to import: Run reports a failure by returning its status, never by calling
// os.Exit.
//
// The test works because the violation is fatal to it. If Run exits the
// process, it takes this test binary with it, and the package reports failure
// with no further output rather than a comparison failure. Nothing else here
// would notice -- cmd/nvkectl/contract_test.go runs the real binary, where
// exiting is correct and indistinguishable.
func TestRunReturnsInsteadOfExiting(t *testing.T) {
	// Not parallel: cobra reads the command line from os.Args.
	original := os.Args
	t.Cleanup(func() { os.Args = original })

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"a command that does not exist", []string{"nvkectl", "no-such-command"}, exitError},
		{"a flag that does not exist", []string{"nvkectl", "version", "--no-such-flag"}, exitError},
		{"a command that succeeds", []string{"nvkectl", "version"}, exitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = tc.args
			if got := Run("test", "test", "test", "test"); got != tc.want {
				t.Errorf("Run() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRunRestoresSignalHandling checks the other half of being importable: Run
// stops diverting SIGINT and SIGTERM once it returns, so the program that
// called it keeps its own handling.
func TestRunRestoresSignalHandling(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"nvkectl", "version"}

	if got := Run("test", "test", "test", "test"); got != exitOK {
		t.Fatalf("Run() = %d, want %d", got, exitOK)
	}

	// Had Run left its own handler installed, this one would not receive the
	// signal and the read below would time out.
	mine := make(chan os.Signal, 1)
	signal.Notify(mine, syscall.SIGUSR1)
	defer signal.Stop(mine)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Skipf("cannot raise a signal here: %v", err)
	}
	select {
	case <-mine:
	case <-time.After(5 * time.Second):
		t.Fatal("the caller's signal handler never fired; Run did not hand the signals back")
	}
}
