// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/cli/internal/util"
	api "github.com/NVIDIA/nke/controller/api"
)

// arrayKeys is the set of --set paths whose value is a comma-separated list
// rather than a scalar, so a single value with no comma is still wrapped as
// a one-element array. This is the only per-field knowledge applySets needs:
// value *types* (string, bool, int, []uuid.UUID, ...) come entirely from the
// generated UpdateClusterRequest struct's own json tags, enforced by the
// json.Decoder in applySets — not hand-listed here. A comma-count heuristic
// alone can't tell a single-element list from a scalar, hence this table.
var arrayKeys = map[string]bool{ //nolint:gochecknoglobals // static lookup table, not mutated after init
	"controlPlaneConfig.nodeLabels":                  true,
	"controlPlaneConfig.nico.instanceTypePreference": true,
	"controlPlaneConfig.nico.sshKeyGroupIDs":         true,
	"controlPlaneConfig.nico.subnetIDs":              true,
	"controlPlaneConfig.nico.vpcPrefixIDs":           true,
	"controlPlaneConfig.infra.preKubeadmCommands":    true,
}

// applySets merges --set entries onto req. Each key is dot notation
// matching UpdateClusterRequest's json field names (e.g.
// "controlPlaneConfig.nico.subnetIDs"). Entries are assembled into a nested
// JSON document — the same shape a -f patch file would decode from — and
// applied with the same json.Decoder path loadCreateClusterRequest uses for
// -f, so the generated struct's json tags decide which keys are valid and
// what type each value must parse as. Unknown keys or values that don't fit
// their field's type surface as the decoder's own errors.
//
// Decoding directly onto req (rather than into a fresh struct) merges into
// whatever -f already populated: encoding/json only overwrites fields
// present in the JSON document, so a key --set doesn't mention keeps its -f
// value, and a key both set and -f'd takes the --set value, since this runs
// after -f is decoded.
func applySets(req *api.UpdateClusterRequest, entries []string) error {
	if len(entries) == 0 {
		return nil
	}

	patch := map[string]interface{}{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("--set %q: expected key=value", entry)
		}
		parsed, err := parseSetValue(key, value)
		if err != nil {
			return fmt.Errorf("--set %q: %w", entry, err)
		}
		if err := setPath(patch, strings.Split(key, "."), parsed); err != nil {
			return fmt.Errorf("--set %q: %w", entry, err)
		}
	}

	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encoding --set values: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		return fmt.Errorf("applying --set: %w", err)
	}
	return nil
}

// setPath writes value into the nested map at the dot-separated path,
// creating intermediate maps as needed.
func setPath(m map[string]interface{}, path []string, value interface{}) error {
	if path[0] == "" {
		return fmt.Errorf("empty key")
	}
	if len(path) == 1 {
		m[path[0]] = value
		return nil
	}
	next, ok := m[path[0]].(map[string]interface{})
	if !ok {
		next = map[string]interface{}{}
		m[path[0]] = next
	}
	return setPath(next, path[1:], value)
}

// parseSetValue coerces a --set value's raw text into the JSON type it most
// likely represents. Keys in arrayKeys always become an array, splitting on
// comma (even a single value with no comma becomes a one-element array,
// since a bare comma count can't distinguish a scalar from a single-element
// list). Otherwise the whole value is parsed as one YAML scalar — the same
// mechanism a -f patch file's values go through — so "true"/"false" become
// booleans, bare integers become numbers, and everything else stays a
// string, without hand-listing which spellings mean which type here.
func parseSetValue(key, raw string) (interface{}, error) {
	if arrayKeys[key] {
		parts := strings.Split(raw, ",")
		out := make([]interface{}, len(parts))
		for i, p := range parts {
			v, err := parseScalar(p)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	return parseScalar(raw)
}

// parseScalar parses one YAML scalar, e.g. "true", "3", "16Gi", or a UUID
// string, into the Go value it represents (bool, int, float64, or string).
func parseScalar(raw string) (interface{}, error) {
	var v interface{}
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", raw, err)
	}
	return v, nil
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
