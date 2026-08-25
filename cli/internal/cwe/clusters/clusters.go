// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clusters provides cobra subcommands for the nkectl cluster resource.
// All commands here are backed by the CWE REST API.
package clusters

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
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
	var outputFormat string

	cmd := &cobra.Command{
		Use:   "clusters",
		Short: "List tenant clusters on the site controller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			clusters, err := c.ListClusters(cmd.Context())
			if err != nil {
				return err
			}
			if f != util.FormatTable {
				return util.Print(os.Stdout, f, clusters)
			}
			printClusters(clusters)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	return cmd
}

// NewDescribeCmd returns the "nkectl describe cluster <name>" subcommand.
func NewDescribeCmd(clientFn ClientFn) *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:   "cluster <name>",
		Short: "Show details of a tenant cluster",
		Args:  cobra.ExactArgs(1),
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
//
// Primary input: a positional spec file whose fields match the CWE
// CreateClusterRequest schema exactly (see /api/docs on your site controller).
// Flags override individual fields — only flags explicitly set on the command
// line take effect; absent flags never clobber values from the spec file.
func NewCreateCmd(clientFn ClientFn) *cobra.Command {
	var (
		name                          string
		controlPlaneKubernetesVersion string
		orgID                         string
	)

	cmd := &cobra.Command{
		Use:   "cluster <spec-file|->",
		Short: "Create a tenant cluster",
		Long: `Create a tenant cluster on the site controller.

The spec must be a YAML document whose fields match the CWE
CreateClusterRequest schema exactly. Validation and default values are
handled by the CWE API — see /api/docs on your site controller.

Pass a file path or '-' to read from stdin (e.g. via a pipe or heredoc).
Flags override the corresponding fields in the spec. Only flags explicitly
set on the command line take effect.

Example — spec file:
  nkectl create cluster examples/capd-cluster.yaml

Example — stdin via pipe:
  cat examples/capd-cluster.yaml | nkectl create cluster -

Example — override the cluster name at runtime:
  nkectl create cluster examples/capd-cluster.yaml --name prod-cluster`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := loadCreateClusterRequest(cmd, args[0])
			if err != nil {
				return err
			}

			applyCreateClusterOverrides(cmd, &req, name, controlPlaneKubernetesVersion, orgID)

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

	cmd.Flags().StringVar(&name, "name", "", "Override: cluster name")
	cmd.Flags().StringVar(&controlPlaneKubernetesVersion, "controlPlaneKubernetesVersion", "", "Override: Kubernetes version (e.g. 1.34.2)")
	cmd.Flags().StringVar(&orgID, "orgID", "", "Override: NGC organization ID")

	return cmd
}

// applyCreateClusterOverrides applies CLI flag values onto req.
// cmd.Flags().Changed() is true only when a flag was explicitly set,
// so absent flags never overwrite values loaded from the YAML file.
// This mirrors the mergeSpec pattern used in nkectl site bootstrap (MR 931).
func applyCreateClusterOverrides(cmd *cobra.Command, req *api.CreateClusterJSONRequestBody, name, k8sVersion, orgID string) {
	flagSet := cmd.Flags().Changed
	if flagSet("name") {
		req.Name = name
	}
	if flagSet("controlPlaneKubernetesVersion") {
		req.ControlPlaneKubernetesVersion = k8sVersion
	}
	if flagSet("orgID") {
		req.OrgID = orgID
	}
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
	var yes bool
	cmd := &cobra.Command{
		Use:   "cluster <name>",
		Short: "Delete a tenant cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "Delete cluster %q? This cannot be undone. [y/N] ", name)
				scanner := bufio.NewScanner(cmd.InOrStdin())
				if !scanner.Scan() {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
				if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}
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
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
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
