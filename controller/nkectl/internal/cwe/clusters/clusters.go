// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary

// Package clusters provides cobra subcommands for the nkectl cluster resource.
// All commands here are backed by the CWE REST API.
package clusters

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	api "github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/nkectl/internal/client"
	"github.com/NVIDIA/nke/controller/nkectl/internal/cwe"
)

// ClientFn is a factory that returns an authenticated CWE client.
// Commands receive this rather than a constructed client so that the
// client is only built when a command actually runs (not at startup).
type ClientFn func() (*client.CWEClient, error)

// NewGetCmd returns the "nkectl get clusters" subcommand.
func NewGetCmd(clientFn ClientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "clusters",
		Short: "List tenant clusters on the site controller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := clientFn()
			if err != nil {
				return err
			}
			clusters, err := c.ListClusters(cmd.Context())
			if err != nil {
				return err
			}
			printClusters(clusters)
			return nil
		},
	}
}

// NewDescribeCmd returns the "nkectl describe cluster <name>" subcommand.
func NewDescribeCmd(clientFn ClientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "cluster <name>",
		Short: "Show details of a tenant cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			printClusterDetail(cl2)
			return nil
		},
	}
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
	)

	cmd := &cobra.Command{
		Use:   "cluster [spec-file]",
		Short: "Create a tenant cluster",
		Long: `Create a tenant cluster on the site controller.

The spec file must be a YAML document whose fields match the CWE
CreateClusterRequest schema exactly. Validation and default values are
handled by the CWE API — see /api/docs on your site controller.

Flags override the corresponding fields in the spec file. Only flags
explicitly set on the command line take effect.

Example — spec file only:
  nkectl create cluster examples/capd-cluster.yaml

Example — override the cluster name at runtime:
  nkectl create cluster examples/capd-cluster.yaml --name prod-cluster`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("spec file is required\n\nUsage: nkectl create cluster [spec-file]")
			}

			req, err := loadCreateClusterRequest(args[0])
			if err != nil {
				return err
			}

			applyCreateClusterOverrides(cmd, &req, name, controlPlaneKubernetesVersion)

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

	return cmd
}

// applyCreateClusterOverrides applies CLI flag values onto req.
// cmd.Flags().Changed() is true only when a flag was explicitly set,
// so absent flags never overwrite values loaded from the YAML file.
// This mirrors the mergeSpec pattern used in nkectl site bootstrap (MR 931).
func applyCreateClusterOverrides(cmd *cobra.Command, req *api.CreateClusterJSONRequestBody, name, k8sVersion string) {
	flagSet := cmd.Flags().Changed
	if flagSet("name") {
		req.Name = name
	}
	if flagSet("controlPlaneKubernetesVersion") {
		req.ControlPlaneKubernetesVersion = k8sVersion
	}
}

// loadCreateClusterRequest reads a YAML file and deserialises it into a
// CreateClusterJSONRequestBody using the shared cwe.YAMLToRequest helper.
func loadCreateClusterRequest(path string) (api.CreateClusterJSONRequestBody, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, fmt.Errorf("reading %q: %w", path, err)
	}
	return cwe.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
}

// NewDeleteCmd returns the "nkectl delete cluster <name>" subcommand.
func NewDeleteCmd(clientFn ClientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "cluster <name>",
		Short: "Delete a tenant cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFn()
			if err != nil {
				return err
			}
			cl, err := c.FindClusterByName(cmd.Context(), args[0])
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
}

// printClusterDetail writes a detailed view of a single cluster to stdout.
func printClusterDetail(cl *api.Cluster) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", cl.Name)
	fmt.Fprintf(w, "ID:\t%s\n", cl.ID)
	fmt.Fprintf(w, "Kubernetes Version:\t%s\n", cl.ControlPlaneKubernetesVersion)
	fmt.Fprintf(w, "CNI:\t%s %s\n", cl.CniPlugin, cl.CniVersion)
	fmt.Fprintf(w, "Etcd Encrypted:\t%v\n", cl.EtcdEncrypted)
	if cl.ControlPlaneVIP != nil {
		fmt.Fprintf(w, "Control Plane VIP:\t%s\n", *cl.ControlPlaneVIP)
	}
	if cl.TeleportEnabled != nil {
		fmt.Fprintf(w, "Teleport Enabled:\t%v\n", *cl.TeleportEnabled)
	}
	fmt.Fprintf(w, "Created:\t%s\n", cl.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "Status:\t%s\n", clusterStatus(cl))
	w.Flush() //nolint:errcheck
}

// clusterStatus returns a human-readable status string derived from conditions.
func clusterStatus(cl *api.Cluster) string {
	c := cl.Conditions
	if c.Deleting.Status == "True" {
		return "Deleting"
	}
	if c.Provisioning.Status == "True" {
		return "Provisioning"
	}
	if c.Provisioned.Status == "True" {
		return "Provisioned"
	}
	return "Unknown"
}

// printClusters writes a tabular summary of clusters to stdout.
func printClusters(clusters []api.Cluster) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tKUBERNETES VERSION\tCNI\tID")
	for _, cl := range clusters {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			cl.Name,
			cl.ControlPlaneKubernetesVersion,
			cl.CniPlugin,
			cl.ID,
		)
	}
	w.Flush() //nolint:errcheck
}
