// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage nvkectl configuration",
	}
	cmd.AddCommand(newConfigSetSiteCmd())
	cmd.AddCommand(newConfigUseSiteCmd())
	cmd.AddCommand(newConfigViewCmd())
	return cmd
}

func newConfigSetSiteCmd() *cobra.Command {
	var url, kubeconfig, name, proxyClientCert, proxyClientKey string
	cmd := &cobra.Command{
		Use:   "set-site",
		Short: "Configure the site controller the CLI connects to",
		Long: `Store the URL and management cluster kubeconfig for a site controller in ~/.config/nvkectl/config.yaml.

For sites fronted by a proxy that requires TLS client authentication, pass
--proxy-client-cert and --proxy-client-key. nvkectl presents the certificate to
the proxy on every request.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if url == "" {
				return fmt.Errorf("--url is required")
			}
			if kubeconfig == "" {
				return fmt.Errorf("--kubeconfig is required")
			}
			if name == "" {
				return fmt.Errorf("--name must not be empty")
			}
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg.Sites[name] = config.SiteConfig{
				URL:                 url,
				Kubeconfig:          kubeconfig,
				ProxyClientCertFile: proxyClientCert,
				ProxyClientKeyFile:  proxyClientKey,
			}
			cfg.CurrentSite = name
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Site %q configured (url: %s)\n", name, url)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Controller API base URL (e.g. http://localhost:8080)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the management cluster kubeconfig")
	cmd.Flags().StringVar(&name, "name", "default", "Name for this site entry")
	cmd.Flags().StringVar(&proxyClientCert, "proxy-client-cert", "", "Path to the proxy TLS client certificate")
	cmd.Flags().StringVar(&proxyClientKey, "proxy-client-key", "", "Path to the private key matching --proxy-client-cert")
	cmd.MarkFlagsRequiredTogether("proxy-client-cert", "proxy-client-key")
	return cmd
}

func newConfigUseSiteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-site <name>",
		Short: "Switch the active site to a previously configured entry",
		Long:  `Set current-site in ~/.config/nvkectl/config.yaml to a previously registered site, without re-registering it.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if _, ok := cfg.Sites[name]; !ok {
				return fmt.Errorf("site %q not found in config: run 'nvkectl config view' to list configured sites", name)
			}
			cfg.CurrentSite = name
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Active site set to %q\n", name)
			return nil
		},
	}
}

func newConfigViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "view",
		Short: "Print the current nvkectl configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return yaml.NewEncoder(cmd.OutOrStdout()).Encode(cfg)
		},
	}
}
