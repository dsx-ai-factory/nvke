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
	jsonpatch "gopkg.in/evanphx/json-patch.v4"

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

// NewUpdateCmd returns the "nkectl update cluster <name>" subcommand.
func NewUpdateCmd(clientFn ClientFn) *cobra.Command {
	var (
		filename   string
		sets       []string
		setStrings []string
	)

	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Update a tenant cluster",
		Long: `Update a tenant cluster on the site controller.

PATCH semantics apply: only the fields you supply are changed. Use --set for
one or two fields, -f for a full patch document, or both together.

Example — bump the Kubernetes version:
  nkectl update cluster my-cluster --set controlPlaneKubernetesVersion=1.31.0

Example — clear a field back to its default:
  nkectl update cluster my-cluster --set controlPlaneConfig.infra.preKubeadmCommands=

Example — patch file:
  nkectl update cluster my-cluster -f patch.yaml

Example — patch file plus an inline override:
  nkectl update cluster my-cluster -f patch.yaml --set controlPlaneConfig.desiredCount=5
  # both fields are applied; if they overlap, --set wins

List fields take comma-separated values (escape a literal comma as \,).
Use --set-string instead of --set to force a value like 1.31 to stay text
rather than become a number; it wins over --set on the same field.

A -f patch document's fields must match the CWE UpdateClusterRequest schema
exactly. See /api/docs on your site controller.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename == "" && len(sets) == 0 && len(setStrings) == 0 {
				return fmt.Errorf("nothing to update: pass --set, --set-string, or -f")
			}
			req, err := loadUpdateClusterRequest(cmd, filename, sets, setStrings)
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
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Set a field: key=value (repeatable, dot notation for nested fields, comma-separated for lists); an empty value clears the field")
	cmd.Flags().StringArrayVar(&setStrings, "set-string", nil, "Like --set, but keeps the value as literal text instead of a number or boolean")
	return cmd
}

// loadUpdateClusterRequest builds the raw JSON PATCH body from an optional
// YAML patch file, --set entries, and --set-string entries, combining the
// three as JSON merge patches (RFC 7386: shared keys merge for nested
// objects, replace wholesale otherwise) in that precedence order, file
// lowest and --set-string highest. The combined document is validated by
// decoding it into UpdateClusterRequest with unknown fields disallowed; the
// decoded value is discarded, and the raw bytes are returned.
func loadUpdateClusterRequest(cmd *cobra.Command, path string, sets, setStrings []string) ([]byte, error) {
	data := []byte("{}")
	if path != "" {
		var r io.Reader
		if path == "-" {
			r = cmd.InOrStdin()
		} else {
			f, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("opening %q: %w", path, err)
			}
			defer f.Close()
			r = f
		}
		fileData, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("reading patch: %w", err)
		}
		fileMap, err := util.YAMLToGenericMap(fileData)
		if err != nil {
			return nil, err
		}
		if data, err = json.Marshal(fileMap); err != nil {
			return nil, fmt.Errorf("encoding patch: %w", err)
		}
	}

	setPatch, err := util.BuildPatch[api.UpdateClusterRequest](sets)
	if err != nil {
		return nil, err
	}
	setData, err := json.Marshal(setPatch)
	if err != nil {
		return nil, fmt.Errorf("encoding patch: %w", err)
	}
	if data, err = jsonpatch.MergeMergePatches(data, setData); err != nil {
		return nil, fmt.Errorf("merging patch: %w", err)
	}

	setStringPatch, err := util.BuildPatchStrings[api.UpdateClusterRequest](setStrings)
	if err != nil {
		return nil, err
	}
	setStringData, err := json.Marshal(setStringPatch)
	if err != nil {
		return nil, fmt.Errorf("encoding patch: %w", err)
	}
	if data, err = jsonpatch.MergeMergePatches(data, setStringData); err != nil {
		return nil, fmt.Errorf("merging patch: %w", err)
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("merging patch: %w", err)
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("nothing to update: the merged patch is empty")
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
