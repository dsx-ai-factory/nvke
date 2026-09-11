// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
)

func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update a resource",
	}
	cmd.AddCommand(clusters.NewUpdateCmd(loadClient))
	return cmd
}
