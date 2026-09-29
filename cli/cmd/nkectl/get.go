// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/cwedeploy"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nke/cli/internal/cwe/clusters"
	"github.com/dsx-ai-factory/nke/cli/internal/cwe/nodepools"
	"github.com/dsx-ai-factory/nke/cli/internal/mgmtcluster"
	mgmtcommand "github.com/dsx-ai-factory/nke/cli/internal/mgmtcluster/command"
	providercmd "github.com/dsx-ai-factory/nke/cli/internal/providers/command"
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
	cmd.AddCommand(newGetBootstrapManifestCmd())
	return cmd
}

// newGetBootstrapManifestCmd is local: it reads the embedded manifest and
// needs no site.
func newGetBootstrapManifestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap-manifest",
		Short: "Print every chart and image the bootstrap pulls, with version and digest",
		Long: `Print the bootstrap manifest this nkectl was built with to standard output.

The manifest lists every chart and image "nkectl bootstrap site" pulls, with
the version and digest of each. nkectl pins it. Use it to see what a
bootstrap fetches, or to populate a registry.

The controller chart and the Postgres chart are not listed. They ship inside
nkectl. Nothing fetches them.`,
		Example:      `  nkectl get bootstrap-manifest`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(manifest.Bytes())
			return err
		},
	}
}

// newMgmtClient adapts mgmtcluster.New's concrete return type to
// mgmtcommand.MgmtClientFactory.
func newMgmtClient(kubeconfigPath string) (mgmtcommand.KubeconfigFetcher, error) {
	return mgmtcluster.New(kubeconfigPath)
}
