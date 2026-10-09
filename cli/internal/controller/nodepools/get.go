// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/clients"
	"github.com/dsx-ai-factory/nke/cli/internal/clusterstate"
	"github.com/dsx-ai-factory/nke/cli/internal/util"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

// NewGetCmd returns the "nkectl get nodepools" subcommand. Both filters are
// applied by the controller, not locally.
func NewGetCmd(clientFn ControllerClientFn) *cobra.Command {
	var (
		outputFormat string
		clusterName  string
	)

	cmd := &cobra.Command{
		Use:     "nodepools [name ...]",
		Aliases: []string{"nodepool"},
		Short:   "List node pools on the site controller",
		Long: `List node pools on the site controller.

Naming pools narrows the listing to them, in the order given. Node pool names
are unique within a cluster, so a name on its own can match a pool in every
cluster; pass --cluster as well to select one.

Example — every pool of one cluster:
  nkectl get nodepools --cluster my-cluster

Example — two pools by name:
  nkectl get nodepools control-plane gpu-workers --cluster my-cluster

Example — one pool, as YAML:
  nkectl get nodepools gpu-workers --cluster my-cluster -o yaml`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			if clusterName != "" {
				if _, err := c.FindClusterByName(cmd.Context(), clusterName); err != nil {
					return err
				}
			}
			pools, missing, err := listNodePools(cmd, c, clusterName, args)
			if err != nil {
				return err
			}
			if f != util.FormatTable {
				if err := util.Print(cmd.OutOrStdout(), f, pools); err != nil {
					return err
				}
				return notFoundError(missing, clusterName)
			}
			names, err := clusterNamesFor(cmd, c, pools, clusterName)
			if err != nil {
				return err
			}
			printNodePools(cmd.OutOrStdout(), pools, names)
			return notFoundError(missing, clusterName)
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	cmd.Flags().StringVarP(&clusterName, "cluster", "c", "", "Filter output to node pools of the named cluster")
	return cmd
}

// listNodePools returns the pools matching names, in the order given, plus the
// names that matched nothing. No names lists every pool of the filtered scope.
func listNodePools(
	cmd *cobra.Command,
	c *clients.ControllerClient,
	clusterName string,
	names []string,
) (pools []api.NodePool, missing []string, err error) {
	params := &api.ListNodePoolsParams{}
	if clusterName != "" {
		params.ClusterName = &clusterName
	}
	if len(names) == 0 {
		pools, err = c.ListNodePools(cmd.Context(), params)
		return pools, nil, err
	}
	for _, name := range names {
		query := *params
		query.Name = &name
		found, err := c.ListNodePools(cmd.Context(), &query)
		if err != nil {
			return nil, nil, err
		}
		if len(found) == 0 {
			missing = append(missing, name)
			continue
		}
		pools = append(pools, found...)
	}
	return pools, missing, nil
}

// notFoundError reports the names that matched nothing, or nil if all did.
func notFoundError(missing []string, clusterName string) error {
	if len(missing) == 0 {
		return nil
	}
	quoted := make([]string, len(missing))
	for i, name := range missing {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	subject := "node pool " + quoted[0]
	if len(quoted) > 1 {
		subject = "node pools " + strings.Join(quoted, ", ")
	}
	if clusterName != "" {
		return fmt.Errorf("%s not found in cluster %q", subject, clusterName)
	}
	return fmt.Errorf("%s not found", subject)
}

// clusterNamesFor maps each pool's cluster ID to its name for the CLUSTER
// column, at the cost of one ListClusters call. None is needed when --cluster
// already names the cluster, or when there are no pools.
func clusterNamesFor(
	cmd *cobra.Command,
	c *clients.ControllerClient,
	pools []api.NodePool,
	clusterFilter string,
) (map[uuid.UUID]string, error) {
	names := map[uuid.UUID]string{}
	if len(pools) == 0 {
		return names, nil
	}
	if clusterFilter != "" {
		for _, p := range pools {
			names[p.ClusterID] = clusterFilter
		}
		return names, nil
	}
	clusters, err := c.ListClusters(cmd.Context(), nil)
	if err != nil {
		return nil, err
	}
	for _, cl := range clusters {
		names[cl.ID] = cl.Name
	}
	return names, nil
}

// nodePoolStatus derives a display state from a pool's conditions. Deletion
// outranks whatever the pool was doing before it.
func nodePoolStatus(pool *api.NodePool) string {
	c := pool.Conditions
	switch {
	case clusterstate.IsTrue(&c.Deleting):
		return "Deleting"
	case clusterstate.IsTrue(&c.Provisioning):
		return "Provisioning"
	case clusterstate.IsTrue(&c.Provisioned):
		return "Provisioned"
	default:
		return "Unknown"
	}
}

// printNodePools writes a tabular summary of pools to out. A pool whose
// cluster is missing from names falls back to printing its cluster ID.
// There is no AGE column: api.NodePool carries no creation timestamp.
func printNodePools(out io.Writer, pools []api.NodePool, names map[uuid.UUID]string) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tCLUSTER\tDESIRED\tK8S VERSION\tCP\tSTATUS\tID")
	for _, p := range pools {
		cluster, ok := names[p.ClusterID]
		if !ok {
			cluster = p.ClusterID.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%t\t%s\t%s\n",
			p.Name,
			cluster,
			p.DesiredNodeCount,
			p.KubernetesVersion,
			p.ControlPlane,
			nodePoolStatus(&p),
			p.ID,
		)
	}
	w.Flush()
}
