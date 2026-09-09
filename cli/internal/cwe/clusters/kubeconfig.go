// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/config"
)

// SiteFn is a factory that resolves the active site's configuration.
//
// Commands take this rather than a resolved config so that the site is only
// looked up when a command actually runs, leaving --help working on a machine
// with no site configured at all.
type SiteFn func() (config.SiteConfig, error)

// KubeconfigFetcher is the subset of *mgmtcluster.Client NewGetKubeconfigCmd
// depends on. Narrow on purpose, so tests can inject a fake without a real
// management cluster kubeconfig or a live server.
type KubeconfigFetcher interface {
	GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error)
}

// MgmtClientFactory builds a KubeconfigFetcher from a management cluster
// kubeconfig path. The caller typically wraps mgmtcluster.New, whose
// *mgmtcluster.Client return type satisfies KubeconfigFetcher.
type MgmtClientFactory func(kubeconfigPath string) (KubeconfigFetcher, error)

// NewGetKubeconfigCmd returns "nkectl get kubeconfig <cluster-name>".
//
// It reads the admin kubeconfig for the named tenant cluster directly from
// the management cluster, using the active site's configured kubeconfig
// (--site overrides which site is active, the same as every other command).
func NewGetKubeconfigCmd(siteFn SiteFn, newMgmtClient MgmtClientFactory) *cobra.Command {
	var outputFile string

	cmd := &cobra.Command{
		Use:   "kubeconfig <cluster-name>",
		Short: "Get the admin kubeconfig for a tenant cluster",
		Long: `Get the admin kubeconfig for a tenant cluster from the management cluster.

The kubeconfig is read from the Secret that the NKE tenantaccess controller
publishes into the management cluster at:

  nke-system/<cluster-name>-admin-kubeconfig  (key: value)

The management cluster kubeconfig is the active site's configured kubeconfig
(see 'nkectl config set-site --kubeconfig'); use --site to target a
different, previously configured site for this invocation.

By default the kubeconfig is printed to stdout. Use --output-file to write it
to a file instead.

Example:
  nkectl get kubeconfig my-cluster
  nkectl get kubeconfig my-cluster --output-file ~/.kube/my-cluster.yaml
  nkectl get kubeconfig my-cluster --site prod`,
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

			data, err := mc.GetClusterKubeconfig(cmd.Context(), clusterName)
			if err != nil {
				return err
			}

			if outputFile != "" {
				// os.WriteFile only applies the mode argument when it creates the
				// file; an existing file (e.g. a pre-existing kubeconfig at 0644)
				// keeps its old, possibly more permissive mode. Chmod explicitly so
				// this kubeconfig's credentials are never left world/group readable.
				if err := os.WriteFile(outputFile, data, 0o600); err != nil {
					return fmt.Errorf("writing kubeconfig to %q: %w", outputFile, err)
				}
				if err := os.Chmod(outputFile, 0o600); err != nil {
					return fmt.Errorf("setting permissions on %q: %w", outputFile, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "kubeconfig written to %s\n", outputFile)
				return nil
			}

			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}

	cmd.Flags().StringVar(&outputFile, "output-file", "", "Write kubeconfig to file instead of stdout")

	return cmd
}
