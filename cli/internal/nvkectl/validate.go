// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

func newValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a file before acting on it",
	}
	cmd.AddCommand(newValidateSiteSpecCmd())
	return cmd
}

type validateSiteSpecOptions struct {
	siteSpec string
}

func newValidateSiteSpecCmd() *cobra.Command {
	opts := &validateSiteSpecOptions{}
	cmd := &cobra.Command{
		Use:   "sitespec",
		Short: "Check a site spec the way nvkectl bootstrap site reads it",
		Long: `Check a site spec: every field is validated. With spire.upstreamCA: true,
NVKECTL_SPIRE_CA_CERT_FILE and NVKECTL_SPIRE_CA_KEY_FILE must both name
readable files; without it, they must be unset. The command prints "valid"
and exits 0 when the file is ready for "nvkectl bootstrap site", and reports
every problem otherwise. It says so when spire.trustDomain is not set, since
the bootstrap and the controller stack install then both use the default.`,
		Example:      `  nvkectl validate sitespec -f sitespec.yaml`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidateSiteSpec(cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "Path to the site spec (required)")
	_ = cmd.MarkFlagRequired("sitespec")
	return cmd
}

func runValidateSiteSpec(out io.Writer, opts *validateSiteSpecOptions) error {
	spec, err := sitespec.Load(opts.siteSpec)
	if err != nil {
		return err
	}
	if domain, defaulted := spec.TrustDomain(); defaulted {
		fmt.Fprintf(out, "spire.trustDomain: not set; a bootstrap with the default cluster name and the controller stack install both use %s\n", domain)
	}
	fmt.Fprintln(out, "valid")
	return nil
}
