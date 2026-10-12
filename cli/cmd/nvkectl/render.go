// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath"
)

func newRenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Write a resolved copy of an input, ready to apply",
	}
	cmd.AddCommand(newRenderDeployPathCmd())
	return cmd
}

type renderDeployPathOptions struct {
	deployPath string
	siteSpec   string
	out        string
}

func newRenderDeployPathCmd() *cobra.Command {
	opts := &renderDeployPathOptions{}
	cmd := &cobra.Command{
		Use:   "deploy-path --deploy-path DIR --sitespec FILE --out DIR",
		Short: "Resolve a deploy path for a site and write the result",
		Long: `Write a copy of a deploy path under --out, resolved for the site the site
spec describes. The committed deploy path is never edited.

The site spec's controllerStack section supplies what the controller stack
needs from the site: the node architecture, the NICo API and its token, the
registry pull secret, the tenant login, and optionally the database
password, a registry mirror, image overrides and an nktp values file. Each
item that takes site values gets a site-values.yaml appended to its values;
the controller gets the database password, the OIDC discovery URL and the
SPIRE server address, SPIRE gets the site identity, the controller's admin
ID, the TPM attestor checksum and the bootstrap CA, PostgreSQL and Temporal
get the database password. Template items are rendered against the site and
become manifest items. The tokens and passwords are read from the files the
site spec names and written into the result, so --out is created 0700 and is
meant for one install.

"nvkectl validate sitespec" checks the site spec first; render refuses the
same problems, every one at once, and leaves nothing under --out on failure.
Then install the result: deploy/nvke-deploy.sh install --deploy-path OUT.`,
		Example:      `  nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --out /tmp/nico-rendered`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRenderDeployPath(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.deployPath, "deploy-path", "", "The committed deploy path (required)")
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "The site spec whose controllerStack section describes the site (required)")
	cmd.Flags().StringVar(&opts.out, "out", "", "Directory to write; must not exist (required)")
	for _, flag := range []string{"deploy-path", "sitespec", "out"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func runRenderDeployPath(cmd *cobra.Command, opts *renderDeployPathOptions) error {
	spec, err := sitespec.Load(opts.siteSpec)
	if err != nil {
		return err
	}
	if _, err := deploypath.Render(deploypath.RenderOptions{DeployPath: opts.deployPath, Site: spec, Out: opts.out, Log: cmd.ErrOrStderr()}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "rendered %s for %s into %s\n", opts.deployPath, spec.ClusterName, opts.out)
	return nil
}
