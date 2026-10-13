// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/operator"
)

var (
	verbose  bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	jsonLogs bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	site     string //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
)

func newRootCmd(ver, commit, treeState, date string) *cobra.Command {
	root := &cobra.Command{
		Use:   "nvkectl",
		Short: "NVKE control-plane CLI",
		// Errors reach the operator from main, which maps them to an exit code
		// at the same time. Leaving cobra to print as well would double every
		// message on stderr.
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return setupLogging(verbose, jsonLogs)
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
func Execute(ctx context.Context, ver, commit, treeState, date string) error {
	return newRootCmd(ver, commit, treeState, date).ExecuteContext(ctx)
}

func setupLogging(verbose, jsonLogs bool) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if jsonLogs {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return nil
}
