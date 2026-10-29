// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvkectl holds the nvkectl command tree.
//
// cmd/nvkectl is a shim over it. The tree lives here, rather than in that
// package, so a second program can import it: the reference documentation
// under cli/docs/reference is generated from the same tree the binary runs,
// and a command tree in package main can be executed but never imported.
//
// Clients are created lazily, when a command runs rather than when the tree is
// built, so constructing the tree reads no configuration and contacts nothing.
package nvkectl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/operator"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

var (
	verbose  bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	jsonLogs bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	site     string //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
)

// NewRootCmd returns the nvkectl command tree, stamped with the build metadata
// the version command reports.
//
// Building the tree reads no configuration and contacts nothing: every client
// is created when a command runs, not here. That is what lets the reference
// documentation be generated from this tree without a kubeconfig, a configured
// site or a reachable cluster.
func NewRootCmd(ver, commit, treeState, date string) *cobra.Command {
	root := &cobra.Command{
		Use:   "nvkectl",
		Short: "NVKE control-plane CLI",
		// Errors reach the operator from main, which maps them to an exit code
		// at the same time. Leaving cobra to print as well would double every
		// message on stderr.
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			run := telemetry.RunFrom(cmd.Context())
			if !isTraced(cmd) {
				run = nil
			}
			var exportErr error
			var cleartextHeaders bool
			if run != nil {
				cfg, ok, strict, err := resolveOTLP(cmd)
				if err == nil && ok {
					err = run.EnableOTLP(cfg)
					cleartextHeaders = err == nil && cfg.Insecure && len(cfg.Headers) > 0
				}
				// An endpoint the operator gave as a flag must work; one from the
				// environment never fails the command.
				if err != nil && strict {
					return err
				}
				exportErr = err
			}
			if err := setupLogging(verbose, jsonLogs, run); err != nil {
				return err
			}
			if exportErr != nil {
				warnExportOff(cmd, exportErr)
			}
			if cleartextHeaders {
				slog.Warn("OTEL_EXPORTER_OTLP_HEADERS is sent to the OTLP endpoint without TLS; use an https endpoint if the headers hold a credential")
			}
			return nil
		},
	}

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.PersistentFlags().BoolVar(&jsonLogs, "json-logs", false, "Emit logs as JSON")
	root.PersistentFlags().StringVar(&site, "site", "", "Override the active site for this invocation")

	root.AddCommand(newVersionCmd(ver, commit, treeState, date))
	root.AddCommand(newConfigCmd())
	root.AddCommand(newBootstrapCmd())
	root.AddCommand(newGenerateCmd())
	root.AddCommand(newRenderCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newGetCmd())
	root.AddCommand(newCreateCmd())
	root.AddCommand(newPatchCmd())
	root.AddCommand(newDeleteCmd())
	root.AddCommand(newDeprecateCmd())
	root.AddCommand(newInspectCmd())

	return root
}

// warnExportOff logs one warning that nvkectl sends no telemetry because the
// endpoint stored with a site, or else the one in the environment, is unusable,
// and what to do. It offers the flag only on a command that has it. The reason
// never holds the endpoint, which may hold credentials.
func warnExportOff(cmd *cobra.Command, reason error) {
	var siteErr *siteEndpointError
	if errors.As(reason, &siteErr) {
		slog.Warn(fmt.Sprintf("nvkectl sends no telemetry because the OTLP endpoint of site %q is not a usable OTLP/gRPC endpoint; "+
			"run nvkectl config set-site again with a valid --%s, or without it to remove the endpoint", siteErr.site, otlpEndpointFlag),
			"reason", reason.Error())
		return
	}
	msg := "nvkectl sends no telemetry because OTEL_EXPORTER_OTLP_ENDPOINT is not a usable OTLP/gRPC endpoint; fix or unset the variable"
	if cmd.Annotations[bootstrapExportAnnotation] == "true" {
		msg += ", or pass --" + otlpEndpointFlag + " to export from this command"
	}
	slog.Warn(msg, "reason", reason.Error())
}

// loadClient is the shared client factory injected into all commands backed by the controller API.
// It is called lazily (when a command runs, not at startup) so that --help and
// non-API commands work without a configured site. If --site was passed, it
// overrides the active site for this invocation only, without touching the
// config file.
func loadClient() (*clients.ControllerClient, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	siteCfg, err := resolveSiteConfig(cfg)
	if err != nil {
		return nil, err
	}
	return clients.New(siteCfg)
}

// loadSite is the shared site-config factory injected into commands that talk
// to the site controller's Kubernetes API rather than to the controller API. Like loadClient
// it is called lazily, so --help works without a configured site.
func loadSite() (config.SiteConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.SiteConfig{}, fmt.Errorf("loading config: %w", err)
	}
	return resolveSiteConfig(cfg)
}

// loadOperator builds the machine image and release client for the active site, from the
// site's management cluster kubeconfig. Like loadSite it is called lazily.
func loadOperator() (operator.Client, error) {
	siteCfg, err := loadSite()
	if err != nil {
		return nil, err
	}
	c, err := operator.NewDynamic(siteCfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// resolveSiteConfig returns the active site's SiteConfig.
func resolveSiteConfig(cfg *config.Config) (config.SiteConfig, error) {
	if site == "" {
		return cfg.CurrentSiteConfig()
	}
	siteCfg, ok := cfg.Sites[site]
	if !ok {
		return config.SiteConfig{}, fmt.Errorf("site %q not found in config", site)
	}
	return siteCfg, nil
}

// Execute runs the command tree and returns the error the command failed with,
// or nil. ctx becomes the root context every command sees through
// cmd.Context(), so cancelling it cancels the command in flight.
//
// Every command but help and completion gets one trace ID. A failing command's
// error ends with it. A succeeding command other than a query command prints it
// as a "Trace ID:" line on stderr, never on stdout. A succeeding query command
// writes nothing and logs the ID at debug level.
func Execute(ctx context.Context, ver, commit, treeState, date string) error {
	root := NewRootCmd(ver, commit, treeState, date)
	return execute(ctx, root, ver, root.ErrOrStderr())
}

// execute runs root under a new trace run and reports the trace ID as Execute
// describes, writing the line for a successful command to stderr.
func execute(ctx context.Context, root *cobra.Command, ver string, stderr io.Writer) error {
	run := telemetry.NewRun(ver)
	// Started before cobra parses anything, so even a rejected flag has an ID.
	// The name is only known once cobra has resolved the command.
	ctx = run.Start(ctx, root.Name())
	cmd, err := root.ExecuteContextC(ctx)
	if cmd != nil {
		run.SetName(cmd.CommandPath())
	}
	// announceTrace logs the command's last record, and Finish shuts the log
	// exporter down, so the announcement comes first.
	result := announceTrace(cmd, stderr, run.TraceID(), err)
	run.Finish(err)
	return result
}

// untracedCommands are the top-level commands that do no work of their own:
// cobra's help and shell completion.
var untracedCommands = map[string]bool{ //nolint:gochecknoglobals // constant lookup table
	"help":                          true,
	"completion":                    true,
	cobra.ShellCompRequestCmd:       true,
	cobra.ShellCompNoDescRequestCmd: true,
}

// queryCommands are the top-level commands that only report or write files.
// When one succeeds it writes nothing but its result, so it does not print the
// trace ID.
var queryCommands = map[string]bool{ //nolint:gochecknoglobals // constant lookup table
	"get":      true,
	"version":  true,
	"config":   true,
	"generate": true,
	"render":   true,
	"inspect":  true,
}

// topLevel returns the name of the command directly under the root that cmd
// belongs to, or "" for the root itself.
func topLevel(cmd *cobra.Command) string {
	path := strings.Fields(cmd.CommandPath())
	if len(path) < 2 {
		return ""
	}
	return path[1]
}

// isTraced reports whether cmd starts a trace: every command but help and
// completion.
func isTraced(cmd *cobra.Command) bool {
	top := topLevel(cmd)
	return top != "" && !untracedCommands[top]
}

// announceTrace reports the run's trace ID for the command that ran. It returns
// err, with the trace ID appended if the command failed. A command that is not
// traced reports nothing.
func announceTrace(cmd *cobra.Command, w io.Writer, traceID string, err error) error {
	if traceID == "" || cmd == nil || !isTraced(cmd) {
		return err
	}
	if err != nil {
		return &tracedError{err: err, traceID: traceID}
	}
	// Cobra prints help for a bare group such as "nvkectl create" and for -h and
	// --help without running anything, so there is nothing to trace.
	if helpRequested(cmd) || !cmd.Runnable() {
		return nil
	}
	if queryCommands[topLevel(cmd)] {
		slog.Debug("command finished", "command", cmd.CommandPath())
		return nil
	}
	fmt.Fprintf(w, "Trace ID: %s\n", traceID)
	return nil
}

// helpRequested reports whether the operator passed -h or --help to cmd. Cobra
// then prints the help and returns success without running the command.
func helpRequested(cmd *cobra.Command) bool {
	help := cmd.Flags().Lookup("help")
	return help != nil && help.Changed
}

// tracedError is a command failure that reports the trace ID of the run.
type tracedError struct {
	err     error
	traceID string
}

func (e *tracedError) Error() string { return e.err.Error() + "\nTrace ID: " + e.traceID }
func (e *tracedError) Unwrap() error { return e.err }

// setupLogging installs the default logger. A run that has started adds its
// trace_id and span_id to every record.
func setupLogging(verbose, jsonLogs bool, run *telemetry.Run) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	// ReplaceAttr masks each value before the handler escapes it, so a credential
	// holding a quote, a backslash or a newline is still found.
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redact.ReplaceAttr}
	var h slog.Handler
	if jsonLogs {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	if run != nil {
		h = run.LogHandler(h)
	}
	slog.SetDefault(slog.New(h))
	return nil
}
