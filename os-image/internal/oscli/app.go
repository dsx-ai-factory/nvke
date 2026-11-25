// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oscli is the command layer shared by nvke-os and nvke-os-node. It
// follows nvkr's CLI conventions: urfave/cli/v2, GNU-style long flags,
// per-command help, and coded errors mapped to banded exit statuses. Commands
// map flags, presentation and errors; behaviour lives in other packages.
package oscli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
)

// NewApp builds a CLI that keeps nvkr's usage contract at every level of the
// command tree. Each binary passes the verbs it ships, so build-only verbs
// never reach the node binary. Verbs add their own flags (--format, --verbose)
// rather than sharing global ones, as nvkr's do.
func NewApp(name, usage, version string, commands ...*cli.Command) *cli.App {
	app := &cli.App{
		Name:            name,
		Usage:           usage,
		Version:         version,
		HideHelpCommand: true,
		OnUsageError:    codedUsageError,
		// Run owns the exit status; urfave/cli must not exit the process from
		// inside a command (it does for an unknown help topic).
		ExitErrHandler: func(*cli.Context, error) {},
		Action: func(c *cli.Context) error {
			if c.Args().Present() {
				return oserr.New(oserr.CodeUsage, "unknown command %q", c.Args().First())
			}
			return cli.ShowAppHelp(c)
		},
		Commands: commands,
	}
	for _, command := range commands {
		configureCommandUsage(command)
	}
	return app
}

// configureCommandUsage applies the usage contract to command and every
// subcommand: a bad flag or a stray argument is a coded usage error, and a verb
// that only groups subcommands rejects an unknown one instead of letting
// urfave/cli exit on its own. Required flags are checked in the action, not
// with urfave's Required, so a missing one is a coded usage error too.
func configureCommandUsage(command *cli.Command) {
	command.OnUsageError = codedUsageError
	if len(command.Subcommands) > 0 {
		if command.Action == nil {
			command.Action = func(c *cli.Context) error {
				if c.Args().Present() {
					return oserr.New(oserr.CodeUsage, "unknown %s subcommand %q", c.Command.Name, c.Args().First())
				}
				return cli.ShowSubcommandHelp(c)
			}
		}
		for _, child := range command.Subcommands {
			configureCommandUsage(child)
		}
		return
	}
	// urfave/cli ignores stray arguments; a leaf verb that declares none
	// refuses them, as nvkectl's do with cobra.NoArgs.
	if command.ArgsUsage == "" {
		command.Before = chainRejectArgs(command.Before)
	}
}

// Run executes app with args (args[0] is the program name), writes results to
// stdout and diagnostics to stderr, and returns the process exit status. Every
// failure carries a code: an uncoded error from a cancelled or timed-out run
// reports CodeTimeout, so a caller can retry it; urfave/cli's own exit errors
// (an unknown help topic) report CodeUsage; anything else reports CodeInternal.
func Run(ctx context.Context, app *cli.App, args []string, stdout, stderr io.Writer) int {
	app.Writer, app.ErrWriter = stdout, stderr
	err := app.RunContext(ctx, args)
	var coded *oserr.Error
	var exitCoder cli.ExitCoder
	switch {
	case err == nil || errors.As(err, &coded):
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		err = oserr.Wrap(oserr.CodeTimeout, err, "run canceled")
	case errors.As(err, &exitCoder):
		err = oserr.Wrap(oserr.CodeUsage, err, "invalid command usage")
	default:
		err = oserr.Wrap(oserr.CodeInternal, err, "command failed")
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
	}
	return oserr.ExitCode(err)
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
