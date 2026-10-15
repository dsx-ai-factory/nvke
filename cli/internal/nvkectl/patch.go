// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/controller/clusters"
	"github.com/dsx-ai-factory/nvke/cli/internal/controller/nodepools"
)

func newPatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "patch",
		Short: "Patch a resource",
	}
	cmd.AddCommand(clusters.NewPatchCmd(loadClient))
	cmd.AddCommand(nodepools.NewPatchCmd(loadClient))
	return cmd
}
