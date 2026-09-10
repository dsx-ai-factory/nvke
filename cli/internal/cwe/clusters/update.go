// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/util"
	api "github.com/NVIDIA/nke/controller/api"
)

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
wins on any key present in both).

Some downstream effects (a rolling Kubernetes upgrade, control-plane node
recycling, etcd encryption enablement) run asynchronously after this command
returns.

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
			fmt.Fprintf(cmd.OutOrStdout(), "Update is async — run 'nkectl get clusters --cluster %s' to check status.\n", updated.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to a YAML patch file, or '-' to read from stdin")
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
