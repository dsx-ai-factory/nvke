// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewDeleteCmd returns the "nvkectl delete nodepool <name>" subcommand.
func NewDeleteCmd(clientFn ControllerClientFn) *cobra.Command {
	var clusterName string

	cmd := &cobra.Command{
		Use:     "nodepool <name> --cluster <cluster>",
		Aliases: []string{"nodepools"},
		Short:   "Delete a node pool",
		Long: `Delete a node pool on the site controller.

Node pool names are unique within a cluster, so --cluster is required to
identify which pool to delete.

Deleting the last joined control-plane node pool of a cluster is rejected;
delete the cluster instead.

Example:
  nvkectl delete nodepool gpu-workers --cluster my-cluster`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFn()
			if err != nil {
				return err
			}
			pool, err := c.FindNodePoolByName(cmd.Context(), clusterName, args[0])
			if err != nil {
				return err
			}
			if err := c.DeleteNodePool(cmd.Context(), pool.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Node pool %q deletion started (id: %s)\n", pool.Name, pool.ID)
			fmt.Fprintf(cmd.OutOrStdout(),
				"Deletion is async — run 'nvkectl get nodepools --cluster %s' to check status.\n", clusterName)
			return nil
		},
	}

	cmd.Flags().StringVarP(&clusterName, "cluster", "c", "", "Name of the cluster that owns the node pool")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}
