// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodepools provides cobra subcommands for the nkectl node pool
// resource. All commands here are backed by the CWE REST API.
package nodepools

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/clients"
)

// ClientFn is a factory that returns an authenticated CWE client.
// Commands receive this rather than a constructed client so that the
// client is only built when a command actually runs (not at startup).
type ClientFn func() (*clients.CWEClient, error)

// NewPatchCmd returns the "nkectl patch nodepool <name>" subcommand.
// It sends the patch document to CWE, leaving every field the document does
// not carry unchanged.
func NewPatchCmd(clientFn ClientFn) *cobra.Command {
	var (
		clusterName string
		patch       string
		patchFile   string
	)

	cmd := &cobra.Command{
		Use:     "nodepool <name> --cluster <cluster>",
		Aliases: []string{"nodepools"},
		Short:   "Patch a node pool",
		Long: `Patch a node pool on the site controller.

Node pool names are unique within a cluster, so --cluster is required to
identify which pool to patch.

PATCH semantics apply: only the fields in the patch document are changed;
everything else on the node pool is left as it is. Pass the patch inline with
--patch, or from a file (or stdin via -) with --patch-file.

nkectl can set ` + strings.Join(settableFields(), ", ") + ` so far. The API
accepts more, but anything else is rejected before the request is sent.

Example — scale a pool:
  nkectl patch nodepool gpu-workers --cluster my-cluster --patch 'desiredNodeCount: 8'

Example — patch file:
  nkectl patch nodepool gpu-workers --cluster my-cluster --patch-file patch.yaml

The patch document's fields must match the CWE UpdateNodePoolRequest schema
exactly. See /api/docs on your site controller.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if patch == "" && patchFile == "" {
				return fmt.Errorf("nothing to patch: pass --patch or --patch-file")
			}
			if patch != "" && patchFile != "" {
				return fmt.Errorf("pass only one of --patch or --patch-file")
			}
			req, err := loadPatchNodePoolRequest(cmd, patch, patchFile)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			pool, err := c.FindNodePoolByName(cmd.Context(), clusterName, args[0])
			if err != nil {
				return err
			}
			updated, err := c.PatchNodePool(cmd.Context(), pool.ID, req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Node pool %q patched (id: %s)\n", updated.Name, updated.ID)
			// No command to point at: the cluster table carries no node pool
			// columns, api.Cluster carries no node pools to print with -o yaml,
			// and get nodepools does not exist yet.
			fmt.Fprintf(cmd.OutOrStdout(), "Update is async — CWE reconciles the pool towards it.\n")
			return nil
		},
	}

	cmd.Flags().StringVarP(&clusterName, "cluster", "c", "", "Name of the cluster that owns the node pool")
	cmd.Flags().StringVarP(&patch, "patch", "p", "", "Inline patch document. Accepts YAML or JSON")
	cmd.Flags().StringVar(&patchFile, "patch-file", "", "Path to a YAML or JSON patch file, or '-' to read from stdin")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}
