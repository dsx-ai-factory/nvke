// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// Build-time variables injected via ldflags.
var (
	version      = "dev"     //nolint:gochecknoglobals // set by ldflags at build time
	gitCommit    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	gitTreeState = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	buildDate    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
)

// The exit codes nvkectl promises its callers. They are part of the CLI's
// compatibility surface, which cli/docs/compatibility.md states and
// cmd/nvkectl/contract_test.go enforces.
const (
	// exitError is returned for every failure that is not a cancellation:
	// invalid input, a usage error, and an API or infrastructure failure alike.
	// nvkectl does not distinguish them by code.
	exitError = 1
	// exitSIGINT and exitSIGTERM are returned when that signal cut the command
	// short, following the shell convention of 128 plus the signal number.
	exitSIGINT  = 128 + int(syscall.SIGINT)
	exitSIGTERM = 128 + int(syscall.SIGTERM)
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	// caught holds the first signal received, or zero if none was.
	var caught atomic.Int32
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	// The first signal cancels the command's context, so a command that honours
	// it unwinds and cleans up rather than dying mid-step. Restoring the default
	// disposition the moment that happens keeps the second signal lethal, so an
	// operation blocked somewhere that does not watch the context — a read from
	// a terminal, a child process — still answers to Ctrl-C.
	go func() {
		sig := <-sigs
		if s, ok := sig.(syscall.Signal); ok {
			caught.Store(int32(s))
		}
		signal.Stop(sigs)
		cancel()
	}()

	err := Execute(ctx, version, gitCommit, gitTreeState, buildDate)
	if err == nil {
		return
	}
	canceled := errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
	os.Exit(reportFailure(os.Stderr, err, canceled, syscall.Signal(caught.Load())))
}

// reportFailure writes err's report to w and returns the exit code. A
// cancellation is the operator's own doing, so it reports as one line rather
// than as a wrapped "context canceled" from wherever the request gave up — but
// not when the error carries recovery instructions, which the operator needs.
// The trace ID is kept either way.
func reportFailure(w io.Writer, err error, canceled bool, sig syscall.Signal) int {
	if !canceled {
		fmt.Fprintln(w, "Error:", err)
		return exitError
	}
	fmt.Fprintln(w, "nvkectl: canceled")
	var recovery *recoveryError
	var traced *tracedError
	switch {
	case errors.As(err, &recovery):
		// err's text already ends with the trace ID.
		fmt.Fprintln(w, "Error:", err)
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
