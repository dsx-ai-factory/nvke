// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
	"github.com/NVIDIA/nke/cli/internal/cwe/nodepools"
	"github.com/NVIDIA/nke/cli/internal/cwedeploy"
	"github.com/NVIDIA/nke/cli/internal/mgmtcluster"
	mgmtcommand "github.com/NVIDIA/nke/cli/internal/mgmtcluster/command"
	providercmd "github.com/NVIDIA/nke/cli/internal/providers/command"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Display one or more resources",
	}
	cmd.AddCommand(clusters.NewGetCmd(loadClient))
	cmd.AddCommand(nodepools.NewGetCmd(loadClient))
	cmd.AddCommand(providercmd.NewGetCmd(loadSite, cwedeploy.ExecCommandRunner{}))
	cmd.AddCommand(mgmtcommand.NewGetKubeconfigCmd(loadSite, newMgmtClient))
	return cmd
}

// newMgmtClient adapts mgmtcluster.New's concrete return type to
// mgmtcommand.MgmtClientFactory.
func newMgmtClient(kubeconfigPath string) (mgmtcommand.KubeconfigFetcher, error) {
	return mgmtcluster.New(kubeconfigPath)
}
