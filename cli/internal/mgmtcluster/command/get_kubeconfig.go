// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package command holds the nvkectl subcommands that read the management
// cluster directly rather than the controller API.
package command

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/oidckubeconfig"
	"github.com/dsx-ai-factory/nvke/cli/internal/util"
)

// SiteFn is a factory that resolves the active site's configuration.
//
// Commands take this rather than a resolved config so that the site is only
// looked up when a command actually runs, leaving --help working on a machine
// with no site configured at all.
type SiteFn func() (config.SiteConfig, error)

// KubeconfigFetcher is the subset of *mgmtcluster.Client NewGetKubeconfigCmd
// depends on.
type KubeconfigFetcher interface {
	GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error)
}

// MgmtClientFactory builds a KubeconfigFetcher from a management cluster
// kubeconfig path. The caller typically wraps mgmtcluster.New, whose
// *mgmtcluster.Client return type satisfies KubeconfigFetcher.
type MgmtClientFactory func(kubeconfigPath string) (KubeconfigFetcher, error)

// NewGetKubeconfigCmd returns "nvkectl get kubeconfig <cluster-name>".
//
// It builds an oidc-login kubeconfig for the named tenant cluster. The cluster
// server and CA come from the admin.conf Secret on the site cluster. The admin
// client certificate is not written.
func NewGetKubeconfigCmd(siteFn SiteFn, newMgmtClient MgmtClientFactory) *cobra.Command {
	var (
		outputFile  string
		issuerURL   string
		clientID    string
		extraScopes []string
	)

	cmd := &cobra.Command{
		Use:   "kubeconfig <cluster-name>",
		Short: "Write an oidc-login kubeconfig for a tenant cluster",
		Long: `Write an oidc-login kubeconfig for a tenant cluster.

The cluster server and CA are read from the admin.conf Secret on the site cluster:

  nke-system/<cluster-name>-admin-kubeconfig  (key: value)

The admin client certificate and key are not copied. The generated user is an
exec plugin:

  kubectl oidc-login get-token --oidc-issuer-url ... --oidc-client-id ...

The site cluster kubeconfig is the active site's configured kubeconfig
(see 'nvkectl config set-site --kubeconfig'); use --site to target a
different, previously configured site for this invocation.

By default the kubeconfig is printed to stdout. Use --output-file to write it
to a file instead.

Example:
  nvkectl get kubeconfig my-cluster \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nke-cli
  nvkectl get kubeconfig my-cluster \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nke-cli \
    --output-file ~/.kube/my-cluster.yaml`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			siteCfg, err := siteFn()
			if err != nil {
				return err
			}

			mc, err := newMgmtClient(siteCfg.Kubeconfig)
			if err != nil {
				return err
			}

			admin, err := mc.GetClusterKubeconfig(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			data, err := oidckubeconfig.GenerateFromKubeconfig(admin, oidckubeconfig.Options{
				IssuerURL:   issuerURL,
				ClientID:    clientID,
				ExtraScopes: extraScopes,
			})
			if err != nil {
				return err
			}

			if outputFile != "" {
				if err := util.WritePrivateFile(outputFile, data); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "kubeconfig written to %s\n", outputFile)
				return nil
			}

			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}

	cmd.Flags().StringVar(&outputFile, "output-file", "", "Write kubeconfig to file instead of stdout")
	cmd.Flags().StringVar(&issuerURL, "oidc-issuer-url", "", "OIDC issuer URL (required)")
	cmd.Flags().StringVar(&clientID, "oidc-client-id", "", "OIDC client ID (required)")
	cmd.Flags().StringArrayVar(&extraScopes, "oidc-extra-scope", nil, "Extra OIDC scope. Repeatable.")
	_ = cmd.MarkFlagRequired("oidc-issuer-url")
	_ = cmd.MarkFlagRequired("oidc-client-id")

	return cmd
}
