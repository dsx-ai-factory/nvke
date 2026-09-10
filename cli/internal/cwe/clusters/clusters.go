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
	"github.com/NVIDIA/nke/cli/internal/clusterstate"
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

// arrayKeys is the set of --set paths whose value is a comma-separated list
// rather than a scalar, so a single value with no comma is still wrapped as
// a one-element array. This is the only per-field knowledge applySets needs:
// value *types* (string, bool, int, []uuid.UUID, ...) come entirely from the
// generated UpdateClusterRequest struct's own json tags, enforced by
// util.ApplySetFlags's json.Decoder — not hand-listed here. util.ApplySetFlags
// can't infer this table itself: given only a value's raw text, it can't
// tell a single-element list field from a scalar field, and it deliberately
// doesn't reflect on UpdateClusterRequest to find out — that would
// reintroduce per-field logic into the generic engine.
var arrayKeys = map[string]bool{ //nolint:gochecknoglobals // static lookup table, not mutated after init
	"controlPlaneConfig.nodeLabels":                  true,
	"controlPlaneConfig.nico.instanceTypePreference": true,
	"controlPlaneConfig.nico.sshKeyGroupIDs":         true,
	"controlPlaneConfig.nico.subnetIDs":              true,
	"controlPlaneConfig.nico.vpcPrefixIDs":           true,
	"controlPlaneConfig.infra.preKubeadmCommands":    true,
}

// applySets merges --set entries onto req using util.ApplySetFlags. See
// arrayKeys for the one piece of UpdateClusterRequest-specific knowledge
// this needs to supply; everything else (dot-path parsing, YAML scalar
// coercion, the merge-decode against req's json tags) is generic.
func applySets(req *api.UpdateClusterRequest, entries []string) error {
	return util.ApplySetFlags(req, entries, arrayKeys)
}

// NewUpdateCmd returns the "nkectl update cluster <name>" subcommand.
func NewUpdateCmd(clientFn ClientFn) *cobra.Command {
	var (
		filename string
		sets     []string
	)

	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Update a tenant cluster",
		Long: `Update a tenant cluster on the site controller.

PATCH semantics apply: only the fields you supply are changed. Use --set for
one or two fields, -f for a full patch document, or both together (--set
wins on any key present in both). A -f patch document's fields must match
the CWE UpdateClusterRequest schema exactly. See /api/docs on your site
controller.

Example — bump the Kubernetes version:
  nkectl update cluster my-cluster --set controlPlaneKubernetesVersion=1.31.0

Example — patch file:
  nkectl update cluster my-cluster -f patch.yaml

Example — file plus an inline override:
  nkectl update cluster my-cluster -f patch.yaml --set controlPlaneConfig.desiredCount=5`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename == "" && len(sets) == 0 {
				return fmt.Errorf("nothing to update: pass --set or -f")
			}
			req, err := loadUpdateClusterRequest(cmd, filename, sets)
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
			updated, err := c.UpdateCluster(cmd.Context(), cl.ID, req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q updated (id: %s)\n", updated.Name, updated.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Update is async — run 'nkectl get clusters' to check status.\n")
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to YAML patch file, or '-' to read from stdin")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Set a field: key=value (repeatable); dot notation for nested fields, comma-separated for lists")
	return cmd
}

// loadUpdateClusterRequest builds an UpdateClusterJSONRequestBody from an
// optional YAML patch file and a list of --set entries. When path is
// non-empty its contents are decoded first; each entry in sets is then
// applied in order, so --set overrides a same-named field loaded from path.
func loadUpdateClusterRequest(cmd *cobra.Command, path string, sets []string) (api.UpdateClusterJSONRequestBody, error) {
	var req api.UpdateClusterJSONRequestBody
	if path != "" {
		var r io.Reader
		if path == "-" {
			r = cmd.InOrStdin()
		} else {
			f, err := os.Open(path)
			if err != nil {
				return req, fmt.Errorf("opening %q: %w", path, err)
			}
			defer f.Close()
			r = f
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return req, fmt.Errorf("reading patch: %w", err)
		}
		req, err = util.YAMLToRequest[api.UpdateClusterJSONRequestBody](data)
		if err != nil {
			return req, err
		}
	}
	if err := applySets(&req, sets); err != nil {
		return req, err
	}
	return req, nil
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
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q deletion started (id: %s)\n", cl.Name, cl.ID)
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
