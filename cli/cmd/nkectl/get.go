// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
	"github.com/NVIDIA/nke/cli/internal/cwedeploy"
	"github.com/NVIDIA/nke/cli/internal/mgmtcluster"
	providercmd "github.com/NVIDIA/nke/cli/internal/providers/command"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Display one or more resources",
	}
	cmd.AddCommand(clusters.NewGetCmd(loadClient))
	cmd.AddCommand(providercmd.NewGetCmd(loadSite, cwedeploy.ExecCommandRunner{}))
	cmd.AddCommand(clusters.NewGetKubeconfigCmd(resolveMgmtKubeconfig, newMgmtClient))
	return cmd
}

// newMgmtClient adapts mgmtcluster.New to clusters.MgmtClientFactory:
// *mgmtcluster.Client satisfies clusters.KubeconfigFetcher, but Go does not
// implicitly convert a function's concrete return type to an interface when
// assigning the function itself, so this wraps the call.
func newMgmtClient(kubeconfigPath string) (clusters.KubeconfigFetcher, error) {
	return mgmtcluster.New(kubeconfigPath)
}
