// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command qualify drives release qualification: plan resolves what to
// qualify, result records what one stream ran, publish ships a stream's
// result and JUnit report to a destination.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// version is stamped at build time. Declared so the -X the Makefile passes
// lands somewhere: the linker ignores an -X for a symbol the binary lacks,
// and it does so silently.
var version = "dev"

// commandTimeout bounds the whole command, most relevantly the GCS calls
// `qualify publish` makes through cmd.Context(). plan and result do not read
// the context at all -- plan's own HEAD requests and git calls are bounded
// by its own 20s client, not this one -- so this only ever caps a stalled
// network call in publish (a hung TLS handshake, a silent partition) from
// blocking indefinitely instead of erroring -- well above any realistic
// publish, which uploads at most three small files with its own bounded
// retry/backoff.
const commandTimeout = 2 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

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
  publish    ship a stream's result and JUnit report to a destination`,
		SilenceUsage: true,
	}
	cmd.AddCommand(planCmd())
	cmd.AddCommand(resultCmd())
	cmd.AddCommand(publishCmd())
	return cmd
}
