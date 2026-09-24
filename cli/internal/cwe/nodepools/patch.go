// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/util"
)

// NewPatchCmd returns the "nkectl patch nodepool <name>" subcommand.
// It sends the patch document to CWE, leaving every field the document does
// not carry unchanged.
func NewPatchCmd(clientFn CWEClientFn) *cobra.Command {
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
			fmt.Fprintf(cmd.OutOrStdout(),
				"Update is async — run 'nkectl get nodepools --cluster %s' to check status.\n", clusterName)
			return nil
		},
	}

	cmd.Flags().StringVarP(&clusterName, "cluster", "c", "", "Name of the cluster that owns the node pool")
	cmd.Flags().StringVarP(&patch, "patch", "p", "", "Inline patch document. Accepts YAML or JSON")
	cmd.Flags().StringVar(&patchFile, "patch-file", "", "Path to a YAML or JSON patch file, or '-' to read from stdin")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

// supportedFields is the subset of UpdateNodePoolRequest this command can
// change. UpdateNodePoolRequest carries more, and CWE accepts them, but only
// what is declared here is supported through nkectl today.
//
// The patch document is validated against this rather than against the full
// request type, which is what rejects an unsupported field. Supporting another
// field is declaring it here and listing it in settableFields.
type supportedFields struct {
	DesiredNodeCount *int `json:"desiredNodeCount,omitempty"`
}

// settableFields lists the fields a patch document may carry, for the
// command's help and for the error an unsupported field produces.
func settableFields() []string {
	return []string{"desiredNodeCount"}
}

// loadPatchNodePoolRequest reads the patch from the inline document or the
// named file ("-" reads stdin) and returns it as the JSON body to send.
func loadPatchNodePoolRequest(cmd *cobra.Command, patch, patchFile string) ([]byte, error) {
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
	// CWE declares no field of the request nullable, so a null would come back
	// as a validator message naming a JSON pointer. Refuse it here instead.
	for field, value := range patchMap {
		if value == nil {
			return nil, fmt.Errorf(
				"%s: null is not a value; omit the field to leave it unchanged", field,
			)
		}
	}
	data, err := json.Marshal(patchMap)
	if err != nil {
		return nil, fmt.Errorf("encoding patch: %w", err)
	}

	var validate supportedFields
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&validate); err != nil {
		return nil, fmt.Errorf("validating patch: %w; nkectl can set %s",
			err, strings.Join(settableFields(), ", "))
	}
	return data, nil
}
