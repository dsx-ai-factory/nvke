// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/controller/clusters"
	"github.com/dsx-ai-factory/nvke/cli/internal/controller/nodepools"
	operatorcmd "github.com/dsx-ai-factory/nvke/cli/internal/operator/command"
)

func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a resource",
	}
	cmd.AddCommand(clusters.NewDeleteCmd(loadClient))
	cmd.AddCommand(nodepools.NewDeleteCmd(loadClient))
	cmd.AddCommand(operatorcmd.NewDeleteCmds(loadOperator)...)
	return cmd
}
