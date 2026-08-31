// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clusters provides cobra subcommands for the nkectl cluster resource.
// All commands here are backed by the CWE REST API.
package clusters

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/util"
	api "github.com/NVIDIA/nke/controller/api"
)

// ClientFn is a factory that returns an authenticated CWE clients.
// Commands receive this rather than a constructed client so that the
// client is only built when a command actually runs (not at startup).
type ClientFn func() (*clients.CWEClient, error)

// NewGetCmd returns the "nkectl get clusters" subcommand.
func NewGetCmd(clientFn ClientFn) *cobra.Command {
	var (
		outputFormat  string
		clusterFilter string
	)

	cmd := &cobra.Command{
		Use:     "clusters",
		Aliases: []string{"cluster"},
		Short:   "List tenant clusters on the site controller",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			var cls []api.Cluster
			if clusterFilter != "" {
				cl, err := c.FindClusterByName(cmd.Context(), clusterFilter)
				if err != nil {
					return err
				}
				cls = []api.Cluster{*cl}
			} else {
				if cls, err = c.ListClusters(cmd.Context()); err != nil {
					return err
				}
			}
			if f != util.FormatTable {
				return util.Print(os.Stdout, f, cls)
			}
			printClusters(cls)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	cmd.Flags().StringVarP(&clusterFilter, "cluster", "c", "", "Filter output to the named cluster")
	return cmd
}

// NewDescribeCmd returns the "nkectl describe cluster <name>" subcommand.
func NewDescribeCmd(clientFn ClientFn) *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Show details of a tenant cluster",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			cl, err := c.FindClusterByName(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cl2, err := c.GetCluster(cmd.Context(), cl.ID)
			if err != nil {
				return err
			}
			if f == util.FormatTable {
				return util.PrintFields(os.Stdout, cl2)
			}
			return util.Print(os.Stdout, f, cl2)
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: key-value)")
	return cmd
}

// NewCreateCmd returns the "nkectl create cluster" subcommand.
func NewCreateCmd(clientFn ClientFn) *cobra.Command {
	var filename string

	cmd := &cobra.Command{
		Use:     "cluster",
		Aliases: []string{"clusters"},
		Short:   "Create a tenant cluster",
		Long: `Create a tenant cluster on the site controller.

The spec must be a YAML document whose fields match the CWE
CreateClusterRequest schema exactly. See /api/docs on your site controller.

Pass a file path or '-' to read from stdin (e.g. via a pipe or heredoc).

Example:
  nkectl create cluster -f examples/capd-cluster.yaml

Example — stdin via pipe:
  cat examples/capd-cluster.yaml | nkectl create cluster -f -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := loadCreateClusterRequest(cmd, filename)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			cluster, err := c.CreateCluster(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Cluster %q created (id: %s)\n", cluster.Name, cluster.ID)
			fmt.Fprintf(os.Stdout, "Provisioning is async — run 'nkectl get clusters' to check status.\n")
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to YAML spec file, or '-' to read from stdin")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// loadCreateClusterRequest reads YAML from path (or stdin when path is "-")
// and deserialises it into a CreateClusterJSONRequestBody.
func loadCreateClusterRequest(cmd *cobra.Command, path string) (api.CreateClusterJSONRequestBody, error) {
	var r io.Reader
	if path == "-" {
		r = cmd.InOrStdin()
	} else {
		f, err := os.Open(path)
		if err != nil {
			return api.CreateClusterJSONRequestBody{}, fmt.Errorf("opening %q: %w", path, err)
		}
		defer f.Close() //nolint:errcheck
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, fmt.Errorf("reading spec: %w", err)
	}
	return util.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
}

// NewDeleteCmd returns the "nkectl delete cluster <name>" subcommand.
func NewDeleteCmd(clientFn ClientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Delete a tenant cluster",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c, err := clientFn()
			if err != nil {
				return err
			}
			cl, err := c.FindClusterByName(cmd.Context(), name)
			if err != nil {
				return err
			}
			if err := c.DeleteCluster(cmd.Context(), cl.ID); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Cluster %q deletion started (id: %s)\n", cl.Name, cl.ID)
			fmt.Fprintf(os.Stdout, "Deletion is async — run 'nkectl get clusters' to check status.\n")
			return nil
		},
	}
	return cmd
}

// clusterStatus returns a human-readable status string derived from conditions.
// Conditions are checked in priority order: terminal/destructive states first,
// then in-progress mutations, then base lifecycle.
func clusterStatus(cl *api.Cluster) string {
	c := cl.Conditions
	switch {
	case conditionIsTrue(&c.Deleting):
		return "Deleting"
	case conditionIsTrue(c.UpdateFailed):
		return "UpdateFailed"
	case c.UpdateInProgress != nil:
		return "Updating"
	case conditionIsTrue(c.ControlPlaneUpgrading):
		return "Upgrading"
	case conditionIsTrue(c.CpNodesRecycling):
		return "Recycling"
	case conditionIsTrue(&c.Provisioning):
		return "Provisioning"
	case conditionIsTrue(&c.Provisioned):
		return "Provisioned"
	default:
		return "Unknown"
	}
}

func conditionIsTrue(condition *api.Condition) bool {
	return condition != nil && condition.Status == api.ConditionStatusTrue
}

// printClusters writes a tabular summary of clusters to stdout.
func printClusters(clusters []api.Cluster) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
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
	w.Flush() //nolint:errcheck
}
