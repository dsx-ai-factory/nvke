// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package command holds the nkectl subcommands that read the management
// cluster directly rather than the controller API.
package command

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/config"
	"github.com/dsx-ai-factory/nke/cli/internal/filesystem"
	"github.com/dsx-ai-factory/nke/cli/internal/oidckubeconfig"
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

// NewGetKubeconfigCmd returns "nkectl get kubeconfig <cluster-name>".
//
// It reads the tenant cluster's server and certificate authority from the
// management cluster, then writes an oidc-login kubeconfig. The admin client
// certificate in that Secret is not copied. The active site's kubeconfig
// selects the management cluster (--site overrides which site is active, the
// same as every other command).
func NewGetKubeconfigCmd(siteFn SiteFn, newMgmtClient MgmtClientFactory) *cobra.Command {
	var (
		outputFile  string
		issuerURL   string
		clientID    string
		extraScopes []string
	)

	cmd := &cobra.Command{
		Use:   "kubeconfig <cluster-name>",
		Short: "Get an oidc-login kubeconfig for a tenant cluster",
		Long: `Get an oidc-login kubeconfig for a tenant cluster.

The cluster server and certificate authority are read from the Secret that
the NKE tenantaccess controller publishes into the management cluster at:

  nke-system/<cluster-name>-admin-kubeconfig  (key: value)

The admin client certificate in that Secret is not copied. The generated
user is an exec plugin:

  kubectl oidc-login get-token --oidc-issuer-url ... --oidc-client-id ...

The management cluster kubeconfig is the active site's configured kubeconfig
(see 'nkectl config set-site --kubeconfig'); use --site to target a
different, previously configured site for this invocation.

By default the kubeconfig is printed to stdout. Use --output-file to write it
to a file instead.

Example:
  nkectl get kubeconfig my-cluster \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nke-cli
  nkectl get kubeconfig my-cluster \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nke-cli \
    --output-file ~/.kube/my-cluster.yaml
  nkectl get kubeconfig my-cluster --site prod \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nke-cli`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clusterName := args[0]

			siteCfg, err := siteFn()
			if err != nil {
				return err
			}

			mc, err := newMgmtClient(siteCfg.Kubeconfig)
			if err != nil {
				return err
			}

			admin, err := mc.GetClusterKubeconfig(cmd.Context(), clusterName)
			if err != nil {
				return err
			}

			data, err := oidckubeconfig.FromKubeconfig(admin, oidckubeconfig.Options{
				IssuerURL:   issuerURL,
				ClientID:    clientID,
				ExtraScopes: extraScopes,
			})
			if err != nil {
				return fmt.Errorf("building oidc kubeconfig for %q: %w", clusterName, err)
			}

			return filesystem.WriteKubeconfig(cmd.OutOrStdout(), outputFile, data)
		},
	}

	cmd.Flags().StringVar(&issuerURL, "oidc-issuer-url", "", "OIDC issuer URL (required)")
	cmd.Flags().StringVar(&clientID, "oidc-client-id", "", "OIDC client ID (required)")
	cmd.Flags().StringArrayVar(&extraScopes, "oidc-extra-scope", nil, "Extra OIDC scope. Repeatable.")
	cmd.Flags().StringVar(&outputFile, "output-file", "", "Write kubeconfig to file instead of stdout")
	_ = cmd.MarkFlagRequired("oidc-issuer-url")
	_ = cmd.MarkFlagRequired("oidc-client-id")

	return cmd
}
