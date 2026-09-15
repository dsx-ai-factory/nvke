// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clusters provides cobra subcommands for the nkectl cluster resource.
// All commands here are backed by the CWE REST API.
package clusters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/clusterstate"
	"github.com/NVIDIA/nke/cli/internal/util"
	api "github.com/NVIDIA/nke/controller/api"
)

// CWEClientFn is a factory that returns an authenticated CWE client.
type CWEClientFn func() (*clients.CWEClient, error)

// NewGetCmd returns the "nkectl get clusters" subcommand.
func NewGetCmd(clientFn CWEClientFn) *cobra.Command {
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
			client, err := clientFn()
			if err != nil {
				return err
			}
			var cls []api.Cluster
			if clusterFilter != "" {
				cluster, err := client.FindClusterByName(cmd.Context(), clusterFilter)
				if err != nil {
					return err
				}
				cls = []api.Cluster{*cluster}
			} else {
				if cls, err = client.ListClusters(cmd.Context()); err != nil {
					return err
				}
			}
			if f != util.FormatTable {
				return util.Print(cmd.OutOrStdout(), f, cls)
			}
			printClusters(cmd.OutOrStdout(), cls)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	cmd.Flags().StringVarP(&clusterFilter, "cluster", "c", "", "Filter output to the named cluster")
	return cmd
}

// NewCreateCmd returns the "nkectl create cluster" subcommand.
func NewCreateCmd(clientFn CWEClientFn) *cobra.Command {
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
			client, err := clientFn()
			if err != nil {
				return err
			}
			cluster, err := client.CreateCluster(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q created (id: %s)\n", cluster.Name, cluster.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Provisioning is async — run 'nkectl get clusters' to check status.\n")
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
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, fmt.Errorf("reading spec: %w", err)
	}
	return util.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
}

// NewPatchCmd returns the "nkectl patch cluster <name>" subcommand.
func NewPatchCmd(clientFn CWEClientFn) *cobra.Command {
	var (
		patch     string
		patchFile string
	)

	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Patch a tenant cluster",
		Long: `Patch a tenant cluster on the site controller.

PATCH semantics apply: only the fields in the patch document are changed;
everything else on the cluster is left as it is. Pass the patch inline with
--patch, or from a file (or stdin via -) with --patch-file.

Example — bump the Kubernetes version:
  nkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'

Example — patch file:
  nkectl patch cluster my-cluster --patch-file patch.yaml

Example — clear a field back to its default:
  nkectl patch cluster my-cluster --patch 'controlPlaneConfig: {nodeLabels: []}'

The patch document's fields must match the CWE UpdateClusterRequest schema
exactly. See /api/docs on your site controller.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if patch == "" && patchFile == "" {
				return fmt.Errorf("nothing to patch: pass --patch or --patch-file")
			}
			if patch != "" && patchFile != "" {
				return fmt.Errorf("pass only one of --patch or --patch-file")
			}
			req, err := loadPatchClusterRequest(cmd, patch, patchFile)
			if err != nil {
				return err
			}
			client, err := clientFn()
			if err != nil {
				return err
			}
			cluster, err := client.FindClusterByName(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			updated, err := client.PatchCluster(cmd.Context(), cluster.ID, req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q patched (id: %s)\n", updated.Name, updated.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Update is async — run 'nkectl get clusters' to check status.\n")
			return nil
		},
	}

	cmd.Flags().StringVarP(&patch, "patch", "p", "", "Inline patch document. Accepts YAML or JSON")
	cmd.Flags().StringVar(&patchFile, "patch-file", "", "Path to a YAML or JSON patch file, or '-' to read from stdin")
	return cmd
}

// loadPatchClusterRequest reads the patch document from patch, or from
// patchFile (a path, or "-" for stdin) when patch is empty, validates it by
// decoding into UpdateClusterRequest with unknown fields disallowed, and
// returns the validated document as raw bytes.
func loadPatchClusterRequest(cmd *cobra.Command, patch, patchFile string) ([]byte, error) {
	raw := []byte(patch)
	if patch == "" {
		var r io.Reader
		if patchFile == "-" {
			r = cmd.InOrStdin()
		} else {
			f, err := os.Open(patchFile)
			if err != nil {
				return nil, fmt.Errorf("opening %q: %w", patchFile, err)
			}
			defer f.Close()
			r = f
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("reading patch: %w", err)
		}
		raw = data
	}

	patchMap, err := util.YAMLToGenericMap(raw)
	if err != nil {
		return nil, err
	}
	if len(patchMap) == 0 {
		return nil, fmt.Errorf("nothing to patch: the patch document is empty")
	}
	data, err := json.Marshal(patchMap)
	if err != nil {
		return nil, fmt.Errorf("encoding patch: %w", err)
	}

	var validate api.UpdateClusterRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&validate); err != nil {
		return nil, fmt.Errorf("validating patch: %w", err)
	}
	return data, nil
}

// NewDeleteCmd returns the "nkectl delete cluster <name>" subcommand.
func NewDeleteCmd(clientFn CWEClientFn) *cobra.Command {
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
			fmt.Fprintf(cmd.OutOrStdout(), "Deletion is async — run 'nkectl get clusters' to check status.\n")
			return nil
		},
	}
	return cmd
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
