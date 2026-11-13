// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oscli is the command layer shared by nvke-os and nvke-os-node. It
// follows nvkr's CLI conventions: urfave/cli/v2, GNU-style long flags,
// per-command help, and coded errors mapped to banded exit statuses. Commands
// map flags, presentation and errors; behaviour lives in other packages.
package oscli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
)

const loggerKey = "nvke-os.logger"

// NewApp builds a CLI with the global flags every nvke-os binary shares. Each
// binary passes the verbs it ships, so build-only verbs never reach the node
// binary.
func NewApp(name, usage, version string, commands ...*cli.Command) *cli.App {
	app := &cli.App{
		Name:            name,
		Usage:           usage,
		Version:         version,
		HideHelpCommand: true,
		OnUsageError:    codedUsageError,
		Metadata:        map[string]any{},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Value: "text", Usage: "result format: text or json"},
			&cli.StringFlag{Name: "log-level", Value: "info", Usage: "debug, info, warn, or error"},
			&cli.StringFlag{Name: "log-format", Value: "auto", Usage: "auto, text, or json (auto is json when CI is set)"},
		},
		Before: configure,
		Action: func(c *cli.Context) error {
			if c.Args().Present() {
				return oserr.New(oserr.CodeUsage, "unknown command %q", c.Args().First())
			}
			return cli.ShowAppHelp(c)
		},
		Commands: commands,
	}
	for _, command := range commands {
		command.OnUsageError = codedUsageError
		// urfave/cli ignores stray arguments; a verb that declares none refuses
		// them, as nvkectl's do with cobra.NoArgs. A verb with subcommands is
		// skipped: its Before sees the subcommand name as an argument.
		if command.ArgsUsage == "" && len(command.Subcommands) == 0 {
			command.Before = chainRejectArgs(command.Before)
		}
	}
	return app
}

// Run executes app with args (args[0] is the program name), writes results to
// stdout and diagnostics to stderr, and returns the process exit status.
func Run(ctx context.Context, app *cli.App, args []string, stdout, stderr io.Writer) int {
	app.Writer, app.ErrWriter = stdout, stderr
	err := app.RunContext(ctx, args)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
	}
	return oserr.ExitCode(err)
}

// Logger returns the invocation's logger, configured once from the global
// flags and tagged with a run ID.
func Logger(c *cli.Context) *slog.Logger {
	if logger, ok := c.App.Metadata[loggerKey].(*slog.Logger); ok {
		return logger
	}
	return slog.New(slog.DiscardHandler)
}

// chainRejectArgs refuses stray arguments before running the command's own
// Before, if it has one.
func chainRejectArgs(next cli.BeforeFunc) cli.BeforeFunc {
	return func(c *cli.Context) error {
		if c.Args().Present() {
			return oserr.New(oserr.CodeUsage, "%s takes no arguments, got %q", c.Command.Name, c.Args().First())
		}
		if next != nil {
			return next(c)
		}
		return nil
	}
}

func codedUsageError(_ *cli.Context, err error, _ bool) error {
	return oserr.Wrap(oserr.CodeUsage, err, "invalid command usage")
}

func configure(c *cli.Context) error {
	switch c.String("output") {
	case "text", "json":
	default:
		return oserr.New(oserr.CodeUsage, "--output must be text or json, got %q", c.String("output"))
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.String("log-level"))); err != nil {
		return oserr.Wrap(oserr.CodeUsage, err, "invalid --log-level")
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch format := c.String("log-format"); {
	case format == "json", format == "auto" && os.Getenv("CI") != "":
		handler = slog.NewJSONHandler(c.App.ErrWriter, opts)
	case format == "text", format == "auto":
		if os.Getenv("JOURNAL_STREAM") != "" {
			// journald stamps each line itself, so drop slog's timestamp.
			opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 0 && a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			}
		}
		handler = slog.NewTextHandler(c.App.ErrWriter, opts)
	default:
		return oserr.New(oserr.CodeUsage, "--log-format must be auto, text, or json, got %q", format)
	}
	runID := make([]byte, 8)
	_, _ = rand.Read(runID) // crypto/rand.Read never returns an error.
	logger := slog.New(handler).With("run_id", hex.EncodeToString(runID))
	c.App.Metadata[loggerKey] = logger
	logger.Debug("starting", "command", c.Args().First())
	return nil
}
