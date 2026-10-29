// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
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
	var url, kubeconfig, name, proxyClientCert, proxyClientKey, otlpEndpoint string
	var otlpInsecure bool
	cmd := &cobra.Command{
		Use:   "set-site",
		Short: "Configure the management plane the CLI connects to",
		Long: `Store the URL and management cluster kubeconfig for a management plane in ~/.config/nvkectl/config.yaml.

The entry for --name is replaced by the values given in this call, and the site
becomes the current site. Run 'nvkectl config use-site <name>' to switch between
configured sites.

For sites fronted by a proxy that requires TLS client authentication, pass
--proxy-client-cert and --proxy-client-key. nvkectl presents the certificate to
the proxy on every request.

--otlp-endpoint is optional. It is the OTLP/gRPC endpoint (host:port or an
http(s):// URL; http implies --otlp-insecure) that receives the spans and log
records of commands that use this site. Headers for the endpoint come from
OTEL_EXPORTER_OTLP_HEADERS and are never stored.`,
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
			endpoint, insecure, err := siteOTLP(cmd, otlpEndpoint, otlpInsecure)
			if err != nil {
				return err
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
				OTLPEndpoint:        endpoint,
				OTLPInsecure:        insecure,
			}
			cfg.CurrentSite = name
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Site %q configured (url: %s)\n", name, url)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Controller API base URL, e.g. http://localhost:8080 (required)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the management cluster kubeconfig (required)")
	cmd.Flags().StringVar(&name, "name", "default", "Name for this site entry. Must not be empty")
	cmd.Flags().StringVar(&proxyClientCert, "proxy-client-cert", "", "Path to the proxy TLS client certificate")
	cmd.Flags().StringVar(&proxyClientKey, "proxy-client-key", "", "Path to the private key matching --proxy-client-cert")
	cmd.Flags().StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP/gRPC endpoint (host:port or an http(s):// URL; http implies --otlp-insecure) that receives the spans and log records of commands that use this site (optional)")
	cmd.Flags().BoolVar(&otlpInsecure, "otlp-insecure", false, "Send to --otlp-endpoint without TLS. An http:// endpoint is always insecure")
	cmd.MarkFlagsRequiredTogether("proxy-client-cert", "proxy-client-key")
	return cmd
}

// siteOTLP turns --otlp-endpoint and --otlp-insecure into the values stored in
// the site entry. An empty endpoint means none.
func siteOTLP(cmd *cobra.Command, endpoint string, insecure bool) (string, bool, error) {
	if endpoint == "" {
		if cmd.Flags().Changed("otlp-insecure") {
			return "", false, fmt.Errorf("--otlp-insecure needs --otlp-endpoint")
		}
		return "", false, nil
	}
	parsed, err := telemetry.ParseEndpoint(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("--otlp-endpoint: %w", err)
	}
	// An https endpoint always uses TLS: the flag would be stored and then ignored.
	if parsed.HTTPS && insecure {
		return "", false, fmt.Errorf("--otlp-insecure conflicts with the https scheme of --otlp-endpoint; " +
			"use an http:// endpoint or drop the scheme, or drop --otlp-insecure")
	}
	return parsed.Host, parsed.Insecure || insecure, nil
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
