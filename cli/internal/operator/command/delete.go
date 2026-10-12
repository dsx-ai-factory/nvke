// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/operator"
)

// NewDeleteCmds returns the "nvkectl delete machineimage|clusterrelease|
// nodepoolrelease" subcommands.
func NewDeleteCmds(clientFn ClientFn) []*cobra.Command {
	cmds := make([]*cobra.Command, 0, len(kindInfos))
	for _, k := range kindInfos {
		cmds = append(cmds, newDeleteCmd(k, clientFn))
	}
	return cmds
}

func newDeleteCmd(k kindInfo, clientFn ClientFn) *cobra.Command {
	return &cobra.Command{
		Use:     k.use + " <name>",
		Aliases: []string{k.plural()},
		Short:   "Delete " + k.what,
		Long: fmt.Sprintf(`Delete a %s from the active site's management cluster.

Deletion is immediate and there is no prompt.

nvkectl does not check whether %s the %s,
and anything that does fails to provision or upgrade once it is gone.%s

Example:
  nvkectl delete %s <name>`, k.noun, k.usedBy, k.noun, k.retireAdvice(), k.use),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFn()
			if err != nil {
				return err
			}
			if err := c.Delete(cmd.Context(), k.kind, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %q deleted\n", capitalize(k.noun), args[0])
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Note: nvkectl does not check whether %s this %s.\n", k.usedBy, k.noun)
			return nil
		},
	}
}

// retireAdvice is the help sentence about retiring a resource that is still in
// use. Only releases have a lifecycle to deprecate.
func (k kindInfo) retireAdvice() string {
	if k.kind == operator.KindMachineImage {
		return ""
	}
	return "\n\nTo retire a release that is still in use, deprecate it instead ('nvkectl deprecate')."
}
