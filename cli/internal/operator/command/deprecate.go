// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewDeprecateCmd returns "nkectl deprecate clusterrelease|nodepoolrelease
// <name>". A machine image has no lifecycle and so no subcommand.
func NewDeprecateCmd(clientFn ClientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deprecate",
		Short: "Deprecate a release",
		// Without a RunE cobra prints help and exits 0 for an unknown kind,
		// which a script would read as success. Only releases can be
		// deprecated, so "nkectl deprecate machineimage x" must fail.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return fmt.Errorf("cannot deprecate %q: only clusterrelease and nodepoolrelease have a lifecycle", args[0])
		},
	}
	for _, k := range kindInfos {
		if k.kind.SupportsDeprecation() {
			cmd.AddCommand(newDeprecateKindCmd(k, clientFn))
		}
	}
	return cmd
}

func newDeprecateKindCmd(k kindInfo, clientFn ClientFn) *cobra.Command {
	return &cobra.Command{
		Use:     k.use + " <name>",
		Aliases: []string{k.plural()},
		Short:   "Deprecate a " + k.noun,
		Long: fmt.Sprintf(`Deprecate a %s on the active site's management cluster.

A deprecated release keeps resolving, so clusters and node pools already built
from it keep provisioning and scaling. It is never chosen as a Kubernetes
upgrade target, and new clusters and node pools should not select it. Only
spec.lifecycle.deprecated changes; the release content is immutable.

Example:
  nkectl deprecate %s <name>`, k.noun, k.use),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFn()
			if err != nil {
				return err
			}
			if _, err := c.SetDeprecated(cmd.Context(), k.kind, args[0], true); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %q deprecated\n", capitalize(k.noun), args[0])
			return nil
		},
	}
}
