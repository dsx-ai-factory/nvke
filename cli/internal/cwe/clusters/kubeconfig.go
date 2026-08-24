// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/mgmtcluster"
)

// NewGetKubeconfigCmd returns "nkectl get kubeconfig <cluster-name>".
//
// It reads the admin kubeconfig for the named cluster directly from the
// management cluster (Option B — see NKX-13899). The management cluster
// kubeconfig is resolved in order: --mgmt-kubeconfig flag → KUBECONFIG
// env → ~/.kube/config.
func NewGetKubeconfigCmd() *cobra.Command {
	var (
		mgmtKubeconfig string
		outputFile     string
	)

	cmd := &cobra.Command{
		Use:   "kubeconfig <cluster-name>",
		Short: "Get the admin kubeconfig for a tenant cluster",
		Long: `Get the admin kubeconfig for a tenant cluster from the management cluster.

The kubeconfig is read from the Secret that the NKE tenantaccess controller
publishes into the management cluster at:

  nke-system/<cluster-name>-kubeconfig  (key: value)

The management cluster is accessed via --mgmt-kubeconfig, the KUBECONFIG
environment variable, or ~/.kube/config (in that order).

By default the kubeconfig is printed to stdout. Use --output to write it to a
file instead.

Example:
  nkectl get kubeconfig my-cluster
  nkectl get kubeconfig my-cluster --output ~/.kube/my-cluster.yaml
  nkectl get kubeconfig my-cluster --mgmt-kubeconfig /path/to/mgmt.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clusterName := args[0]

			mc, err := mgmtcluster.New(mgmtKubeconfig)
			if err != nil {
				return err
			}

			data, err := mc.GetClusterKubeconfig(cmd.Context(), clusterName)
			if err != nil {
				return err
			}

			if outputFile != "" {
				if err := os.WriteFile(outputFile, data, 0600); err != nil {
					return fmt.Errorf("writing kubeconfig to %q: %w", outputFile, err)
				}
				fmt.Fprintf(os.Stdout, "kubeconfig written to %s\n", outputFile)
				return nil
			}

			_, err = os.Stdout.Write(data)
			return err
		},
	}

	cmd.Flags().StringVar(&mgmtKubeconfig, "mgmt-kubeconfig", "", "Path to management cluster kubeconfig (default: KUBECONFIG env or ~/.kube/config)")
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "Write kubeconfig to file instead of stdout")

	return cmd
}
