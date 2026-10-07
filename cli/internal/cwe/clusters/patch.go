// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/util"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

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
		data, err := util.ReadFileOrStdin(cmd.InOrStdin(), patchFile, "patch")
		if err != nil {
			return nil, err
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
