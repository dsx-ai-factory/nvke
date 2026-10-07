// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/operator"
	"github.com/dsx-ai-factory/nke/cli/internal/util"
)

const (
	dryRunNone   = "none"
	dryRunClient = "client"
)

// NewCreateCmds returns the "nkectl create machineimage|clusterrelease|
// nodepoolrelease" subcommands.
func NewCreateCmds(clientFn ClientFn) []*cobra.Command {
	cmds := make([]*cobra.Command, 0, len(kindInfos))
	for _, k := range kindInfos {
		cmds = append(cmds, newCreateCmd(k, clientFn))
	}
	return cmds
}

func newCreateCmd(k kindInfo, clientFn ClientFn) *cobra.Command {
	var (
		filename     string
		outputFormat string
		dryRun       string
	)

	cmd := &cobra.Command{
		Use:     k.use,
		Aliases: []string{k.plural()},
		Short:   "Create " + k.what,
		Long: fmt.Sprintf(`Create a %s on the active site's management cluster.

The request is a single YAML or JSON document with a metadata and a spec, and
nothing else. nkectl supplies the apiVersion and kind, so leave them out:

  metadata:
    name: <name>
  spec:
    ...

Pass a file path or '-' to read from stdin. Unknown fields are rejected.

Published content is immutable. To change what a %s provisions, create it
again under a new name; creating an existing name fails. A release must name a
machine image that already exists, so create the machine image first.

With --dry-run=client the document is checked and printed as a complete
manifest, and no management cluster is contacted, so no site is needed. The
check covers structure only: required fields and value rules are enforced by the
management cluster when the resource is created, and the machine image a
release names is not looked up.

Example:
  nkectl create %s -f %s.yaml

Example - print the complete manifest without creating anything:
  nkectl create %s -f %s.yaml --dry-run=client -o yaml`,
			k.noun, k.noun, k.use, k.use, k.use, k.use),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			if dryRun != dryRunNone && dryRun != dryRunClient {
				return fmt.Errorf("invalid --dry-run value %q: must be %q or %q", dryRun, dryRunNone, dryRunClient)
			}
			data, err := readDocument(cmd, filename)
			if err != nil {
				return err
			}
			res, err := operator.Decode(k.kind, data)
			if err != nil {
				return err
			}

			if dryRun == dryRunClient {
				if format == util.FormatTable {
					format = util.FormatYAML
				}
				return util.Print(cmd.OutOrStdout(), format, operator.Manifest(k.kind, res))
			}

			c, err := clientFn()
			if err != nil {
				return err
			}
			if err := checkMachineImageExists(cmd.Context(), c, k, res); err != nil {
				return err
			}
			created, err := c.Create(cmd.Context(), k.kind, res)
			if err != nil {
				return err
			}
			if format != util.FormatTable {
				return util.Print(cmd.OutOrStdout(), format, created)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %q created\n", capitalize(k.noun), created.Metadata.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to the YAML or JSON request, or '-' to read from stdin")
	_ = cmd.MarkFlagRequired("filename")
	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: a one-line message, or yaml with --dry-run)")
	cmd.Flags().StringVar(&dryRun, "dry-run", dryRunNone, "Use \"client\" to print the complete manifest without contacting the management cluster")
	return cmd
}

// checkMachineImageExists fails when a release names a machine image that does
// not exist, so a release is never published pointing at nothing. Machine
// images name nothing and are not checked.
//
// The check and the create are two requests, so an image deleted in between is
// not caught. It reads the machine image, which needs get on machineimages.
func checkMachineImageExists(ctx context.Context, c operator.Client, k kindInfo, res operator.Resource) error {
	if k.kind == operator.KindMachineImage {
		return nil
	}
	ref, _ := res.Spec["machineImageRef"].(map[string]any)
	name, _ := ref["name"].(string)
	if name == "" {
		return nil
	}
	if _, err := c.Get(ctx, operator.KindMachineImage, name); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("machine image %q not found: create it first with 'nkectl create machineimage'", name)
		}
		return fmt.Errorf("checking machine image %q: %w", name, err)
	}
	return nil
}

// capitalize upper-cases the first letter of s.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
