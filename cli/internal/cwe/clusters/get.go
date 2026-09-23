// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/clients"
	"github.com/dsx-ai-factory/nke/cli/internal/clusterstate"
	"github.com/dsx-ai-factory/nke/cli/internal/util"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

// NewGetCmd returns the "nkectl get clusters" subcommand.
func NewGetCmd(clientFn CWEClientFn) *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:     "clusters [name ...]",
		Aliases: []string{"cluster"},
		Short:   "List tenant clusters on the site controller",
		Long: `List tenant clusters on the site controller.

Naming clusters narrows the listing to them, in the order given.

Example — every cluster:
  nkectl get clusters

Example — two clusters, as YAML:
  nkectl get clusters prod staging -o yaml`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			client, err := clientFn()
			if err != nil {
				return err
			}
			cls, missing, err := listClusters(cmd, client, args)
			if err != nil {
				return err
			}
			if f != util.FormatTable {
				if err := util.Print(cmd.OutOrStdout(), f, cls); err != nil {
					return err
				}
				return notFoundError(missing)
			}
			printClusters(cmd.OutOrStdout(), cls)
			return notFoundError(missing)
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	return cmd
}

// listClusters returns the clusters matching names, in the order given, plus
// the names that matched nothing. No names lists every cluster.
func listClusters(
	cmd *cobra.Command,
	client *clients.CWEClient,
	names []string,
) (cls []api.Cluster, missing []string, err error) {
	if len(names) == 0 {
		cls, err = client.ListClusters(cmd.Context(), nil)
		return cls, nil, err
	}
	for _, name := range names {
		found, err := client.ListClusters(cmd.Context(), &api.ListClustersParams{Name: &name})
		if err != nil {
			return nil, nil, err
		}
		if len(found) == 0 {
			missing = append(missing, name)
			continue
		}
		cls = append(cls, found...)
	}
	return cls, missing, nil
}

// notFoundError reports the names that matched nothing, or nil if all did.
func notFoundError(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	quoted := make([]string, len(missing))
	for i, name := range missing {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	if len(quoted) > 1 {
		return fmt.Errorf("clusters %s not found", strings.Join(quoted, ", "))
	}
	return fmt.Errorf("cluster %s not found", quoted[0])
}

// clusterStatus keeps the package-local helper used by the cluster command
// tests while sharing the status rules with site bootstrap.
func clusterStatus(cl *api.Cluster) string { return clusterstate.Status(cl) }

// printClusters writes a tabular summary of clusters to out.
func printClusters(out io.Writer, clusters []api.Cluster) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tKUBERNETES VERSION\tCNI\tSTATUS\tID\tAGE")
	for _, cl := range clusters {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			cl.Name,
			cl.ControlPlaneKubernetesVersion,
			cl.CniPlugin,
			clusterStatus(&cl),
			cl.ID,
			util.FormatAge(cl.CreatedAt),
		)
	}
	w.Flush()
}
