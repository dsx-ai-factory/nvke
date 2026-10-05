// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Build-time variables injected via ldflags.
var (
	version      = "dev"     //nolint:gochecknoglobals // set by ldflags at build time
	gitCommit    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	gitTreeState = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	buildDate    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
)

// The exit codes nkectl promises its callers. They are part of the CLI's
// compatibility surface, which cli/docs/compatibility.md states and
// cmd/nkectl/contract_test.go enforces.
const (
	// exitError is returned for every failure that is not a cancellation:
	// invalid input, a usage error, and an API or infrastructure failure alike.
	// nkectl does not distinguish them by code.
	exitError = 1
	// exitCanceled is returned when SIGINT or SIGTERM cut the command short,
	// following the shell convention of 128 plus the signal number for SIGINT.
	exitCanceled = 130
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// The first signal cancels the command's context, so a command that honours
	// it unwinds and cleans up rather than dying mid-step. Restoring the default
	// disposition the moment that happens keeps the second signal lethal, so an
	// operation blocked somewhere that does not watch the context — a read from
	// a terminal, a child process — still answers to Ctrl-C.
	go func() {
		<-ctx.Done()
		stop()
	}()

	err := Execute(ctx, version, gitCommit, gitTreeState, buildDate)
	if err == nil {
		return
	}
	// Cancellation is the operator's own doing, so it reports as one line rather
	// than as a wrapped "context canceled" from wherever the request gave up.
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		fmt.Fprintln(os.Stderr, "nkectl: canceled")
		os.Exit(exitCanceled)
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(exitError)
}
