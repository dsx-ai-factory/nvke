// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// The exit codes nvkectl promises its callers. They are part of the CLI's
// compatibility surface, which cli/docs/compatibility.md states and
// cmd/nvkectl/contract_test.go enforces.
const (
	// exitOK is returned when the command succeeded.
	exitOK = 0
	// exitError is returned for every failure that is not a cancellation:
	// invalid input, a usage error, and an API or infrastructure failure alike.
	// nvkectl does not distinguish them by code.
	exitError = 1
	// exitSIGINT and exitSIGTERM are returned when that signal cut the command
	// short, following the shell convention of 128 plus the signal number.
	exitSIGINT  = 128 + int(syscall.SIGINT)
	exitSIGTERM = 128 + int(syscall.SIGTERM)
)

// Run executes nvkectl and returns the status the process should exit with:
// exitOK, or one of the codes above when the command failed or was cut short
// by a signal. It writes any failure report to standard error.
//
// It returns rather than exiting. Terminating the process belongs to
// cmd/nvkectl, which calls os.Exit on what this returns: os.Exit here would
// take down any program that imported this package, skipping every deferred
// close, flush and cleanup it had pending. For the same reason Run leaves the
// caller's signal handling and context as it found them.
//
// The build-time strings stay in package main, where the linker's -X flags can
// reach them, and are passed in here: for that flag the package name is what
// matches, so a symbol moved into this package would be silently left at its
// default. cli/Makefile asserts the stamp took.
func Run(ver, commit, treeState, date string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// caught holds the first signal received, or zero if none was.
	var caught atomic.Int32
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	// Hand the signals back on the way out. Without this an importing program
	// would find SIGINT and SIGTERM still diverted here after Run returned.
	defer signal.Stop(sigs)

	// done releases the watcher below when Run returns with no signal received.
	// Without it that goroutine blocks on sigs forever, which is invisible in a
	// binary that is about to exit and a leak per call in anything else.
	done := make(chan struct{})
	defer close(done)

	// The first signal cancels the command's context, so a command that honours
	// it unwinds and cleans up rather than dying mid-step. Restoring the default
	// disposition the moment that happens keeps the second signal lethal, so an
	// operation blocked somewhere that does not watch the context — a read from
	// a terminal, a child process — still answers to Ctrl-C.
	go func() {
		select {
		case sig := <-sigs:
			if s, ok := sig.(syscall.Signal); ok {
				caught.Store(int32(s))
			}
			signal.Stop(sigs)
			cancel()
		case <-done:
		}
	}()

	err := Execute(ctx, ver, commit, treeState, date)
	if err == nil {
		return exitOK
	}
	canceled := errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
	return reportFailure(os.Stderr, err, canceled, syscall.Signal(caught.Load()))
}

// reportFailure writes err's report to w and returns the exit code. A
// cancellation is the operator's own doing, so it reports as one line rather
// than as a wrapped "context canceled" from wherever the request gave up — but
// not when the error carries recovery instructions, which the operator needs.
// The trace ID is kept either way.
func reportFailure(w io.Writer, err error, canceled bool, sig syscall.Signal) int {
	if !canceled {
		fmt.Fprintln(w, "Error:", redact.Text(err.Error()))
		return exitError
	}
	fmt.Fprintln(w, "nvkectl: canceled")
	var recovery *recoveryError
	var traced *tracedError
	switch {
	case errors.As(err, &recovery):
		// err's text already ends with the trace ID.
		fmt.Fprintln(w, "Error:", redact.Text(err.Error()))
	case errors.As(err, &traced):
		// An interrupted request may already have reached the controller, so
		// the operator keeps the ID to find out what became of it.
		fmt.Fprintln(w, "Trace ID:", traced.traceID)
	}
	if sig == syscall.SIGTERM {
		return exitSIGTERM
	}
	return exitSIGINT
}
