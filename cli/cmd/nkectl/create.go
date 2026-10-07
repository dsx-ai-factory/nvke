// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/cwe/clusters"
	"github.com/dsx-ai-factory/nke/cli/internal/cwe/nodepools"
	operatorcmd "github.com/dsx-ai-factory/nke/cli/internal/operator/command"
)

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a resource",
	}
	cmd.AddCommand(clusters.NewCreateCmd(loadClient))
	cmd.AddCommand(nodepools.NewCreateCmd(loadClient))
	cmd.AddCommand(operatorcmd.NewCreateCmds(loadOperator)...)
	return cmd
}
