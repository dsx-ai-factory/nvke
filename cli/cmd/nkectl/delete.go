// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
)

func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a resource",
	}
	cmd.AddCommand(clusters.NewDeleteCmd(loadClient))
	return cmd
}
