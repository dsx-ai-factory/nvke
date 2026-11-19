// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command triage names why a qualification job failed.
//
// `triage classify` reads a job's log and the name of the step that failed, and
// prints the class of failure, what may be done about it, and the line of the
// log that decided it. The rules are in release/triage.yaml. It only reports:
// it never re-runs a job, posts a message or touches a machine.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/triage"
)

// version is stamped at build time; see the note on it in cmd/qualify.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "triage:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "triage",
		Short:         "Name why a qualification job failed",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(classifyCmd(), validateCmd())

	return cmd
}

func classifyCmd() *cobra.Command {
	var (
		rules string
		step  string
		log   string
	)

	cmd := &cobra.Command{
		Use:   "classify",
		Short: "Classify a failed job from its log",
		Long: `Classify a failed job from its log.

Give the name of the step that failed and the job's log, as a file or - for
standard input. The result is JSON: the class, the rule that matched, the
action the rules allow, the line of the log that decided it, and a note for a
person. A failure no rule matches is class "unknown" with action "none", and is
not an error.

The log is read for what a failing step wrote, not searched whole: a job log
also holds the workflow's own shell source, which would match by accident.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClassify(cmd.OutOrStdout(), cmd.InOrStdin(), rules, step, log)
		},
	}

	cmd.Flags().StringVar(&rules, "rules", "triage.yaml", "the rules file")
	cmd.Flags().StringVar(&step, "step", "", "the name of the step that failed")
	cmd.Flags().StringVar(&log, "log", "-", "the job's log, or - for standard input")

	return cmd
}

func runClassify(out io.Writer, in io.Reader, rules, step, log string) error {
	c, err := triage.Load(rules)
	if err != nil {
		return err
	}

	r := in

	if log != "-" {
		f, err := os.Open(log)
		if err != nil {
			return fmt.Errorf("opening the log: %w", err)
		}
		defer f.Close()

		r = f
	}

	e, err := triage.Extract(r)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")

	return enc.Encode(c.Classify(step, e))
}

func validateCmd() *cobra.Command {
	var rules string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a rules file without classifying anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := triage.Load(rules)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d rules\n", len(c.Rules))

			return nil
		},
	}

	cmd.Flags().StringVar(&rules, "rules", "triage.yaml", "the rules file")

	return cmd
}
