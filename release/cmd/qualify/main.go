// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command qualify drives release qualification: plan resolves what to
// qualify, result records what one stream ran, publish ships a stream's
// result and JUnit report to a destination, verify checks that what a plan
// names is published, and summary renders a plan or the streams' JUnit
// reports as Markdown for a run page.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// version is stamped at build time. Declared so the -X the Makefile passes
// lands somewhere: the linker ignores an -X for a symbol the binary lacks,
// and it does so silently.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root().ExecuteContext(ctx); err != nil {
		// Cobra has already printed usage errors. Anything else is ours and
		// is printed here, once.
		var silent silentError
		if !errors.As(err, &silent) {
			fmt.Fprintln(os.Stderr, "qualify:", err)
		}
		os.Exit(1)
	}
}

// silentError marks an error cobra has already reported.
type silentError struct{ error }

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "qualify",
		Short:   "Drive NVKE release qualification",
		Version: version,
		Long: `Drive NVKE release qualification.

  plan       resolve what to qualify
  result     record what one stream of one combination qualified
  publish    ship a stream's result and JUnit report to a destination
  verify     check that everything a plan names is published
  summary    render a plan, or the streams' JUnit reports, as Markdown`,
		SilenceUsage: true,
	}
	cmd.AddCommand(planCmd())
	cmd.AddCommand(resultCmd())
	cmd.AddCommand(publishCmd())
	cmd.AddCommand(verifyCmd())
	cmd.AddCommand(summaryCmd())
	return cmd
}
