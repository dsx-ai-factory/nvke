// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command notify sends qualification events to Slack from a config file, so
// who is told about what is configuration and not workflow code.
//
// A job runs `notify trigger <event> --config release/notify.yml` and the
// config decides where it lands. `notify validate` checks a config without
// sending anything.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// version is stamped at build time; see the note on it in cmd/qualify.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "notify:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "notify",
		Short:   "Send qualification events to Slack from configuration",
		Version: version,
		Long: `Send qualification events to Slack from configuration.

  trigger    send what the config calls for on an event
  validate   check a config, and list the keys each notification reads`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(triggerCmd())
	cmd.AddCommand(validateCmd())
	return cmd
}
