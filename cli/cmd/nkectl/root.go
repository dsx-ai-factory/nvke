// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/config"
)

var (
	verbose  bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	jsonLogs bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	site     string //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
)

func newRootCmd(ver, commit, treeState, date string) *cobra.Command {
	root := &cobra.Command{
		Use:          "nkectl",
		Short:        "NKE control-plane CLI",
		SilenceUsage: true,
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
	root.AddCommand(newGetCmd())
	root.AddCommand(newCreateCmd())
	root.AddCommand(newUpdateCmd())
	root.AddCommand(newDeleteCmd())

	return root
}

// loadClient is the shared client factory injected into all CWE-backed commands.
// It is called lazily (when a command runs, not at startup) so that --help and
// non-API commands work without a configured site. If --site was passed, it
// overrides the active site for this invocation only, without touching the
// config file.
func loadClient() (*clients.CWEClient, error) {
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
// to the site controller's Kubernetes API rather than to CWE. Like loadClient
// it is called lazily, so --help works without a configured site.
func loadSite() (config.SiteConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.SiteConfig{}, fmt.Errorf("loading config: %w", err)
	}
	return resolveSiteConfig(cfg)
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

// Execute is the entry point called from main.
func Execute(ver, commit, treeState, date string) error {
	return newRootCmd(ver, commit, treeState, date).Execute()
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
