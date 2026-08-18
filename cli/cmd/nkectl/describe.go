// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
)

func newDescribeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe",
		Short: "Show details of a resource",
	}
	cmd.AddCommand(clusters.NewDescribeCmd(loadClient))
	return cmd
}
