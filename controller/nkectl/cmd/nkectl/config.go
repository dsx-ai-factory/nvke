// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/controller/nkectl/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage nkectl configuration",
	}
	cmd.AddCommand(newConfigSetSiteCmd())
	cmd.AddCommand(newConfigViewCmd())
	cmd.AddCommand(newConfigValidateCmd())
	return cmd
}

func newConfigSetSiteCmd() *cobra.Command {
	var url, name string
	cmd := &cobra.Command{
		Use:   "set-site",
		Short: "Configure the site controller the CLI connects to",
		Long:  `Store the URL for a site controller in ~/.config/nkectl/config.yaml.`,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if url == "" {
				return fmt.Errorf("--url is required")
			}
			if name == "" {
				return fmt.Errorf("--name must not be empty")
			}
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg.Sites[name] = config.SiteConfig{URL: url}
			cfg.CurrentSite = name
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(os.Stdout, "Site %q configured (url: %s)\n", name, url)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "CWE API base URL (e.g. http://localhost:8080)")
	cmd.Flags().StringVar(&name, "name", "default", "Name for this site entry")
	return cmd
}

func newConfigViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "view",
		Short: "Print the current nkectl configuration",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return yaml.NewEncoder(os.Stdout).Encode(cfg)
		},
	}
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [spec-file]",
		Short: "Validate a bootstrap spec file",
		Long: `Load and validate a bootstrap spec file (YAML).

Not yet implemented — the config package is ported in C2.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("config validate: not yet implemented (config package lands in C2)")
		},
	}
}
