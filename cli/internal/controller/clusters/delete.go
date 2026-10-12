// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewDeleteCmd returns the "nvkectl delete cluster <name>" subcommand.
func NewDeleteCmd(clientFn ControllerClientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Delete a tenant cluster",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			client, err := clientFn()
			if err != nil {
				return err
			}
			cluster, err := client.FindClusterByName(cmd.Context(), name)
			if err != nil {
				return err
			}
			if err := client.DeleteCluster(cmd.Context(), cluster.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q deletion started (id: %s)\n", cluster.Name, cluster.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Deletion is async — run 'nvkectl get clusters' to check status.\n")
			return nil
		},
	}
	return cmd
}
