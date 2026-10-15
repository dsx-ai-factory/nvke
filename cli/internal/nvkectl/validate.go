// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"
	"io"
	"strings"

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
	siteSpec      string
	bootstrapOnly bool
}

func newValidateSiteSpecCmd() *cobra.Command {
	opts := &validateSiteSpecOptions{}
	cmd := &cobra.Command{
		Use:   "sitespec",
		Short: "Check a site spec for the bootstrap and the controller stack install",
		Long: `Check the whole site spec and report by section.

The bootstrap section is what "nvkectl bootstrap site" reads: every field is
checked, and the SPIRE CA files must exist. The controllerStack section is
what the controller stack install reads: the REPLACE_* values still to fill
in are listed apart from errors, every required field must be set, the
architecture and the NICo endpoint must be well-formed, and the files the
section names must exist.

The command exits 0 only when the file is ready for both. --bootstrap-only
checks the bootstrap section alone, for a file whose controllerStack section
is not filled in yet.`,
		Example: `  nvkectl validate sitespec -f sitespec.yaml
  nvkectl validate sitespec -f sitespec.yaml --bootstrap-only`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidateSiteSpec(cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "Path to the site spec (required)")
	_ = cmd.MarkFlagRequired("sitespec")
	cmd.Flags().BoolVar(&opts.bootstrapOnly, "bootstrap-only", false, "Check only the fields bootstrap site reads")
	return cmd
}

func runValidateSiteSpec(out io.Writer, opts *validateSiteSpecOptions) error {
	spec, err := sitespec.Load(opts.siteSpec)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "bootstrap: valid; nvkectl bootstrap site can run with this file")
	if opts.bootstrapOnly {
		return nil
	}

	if spec.ControllerStack == nil {
		fmt.Fprintln(out, "controllerStack: missing; nvkectl generate sitespec writes the section to fill in")
		return errors.New("the site spec is not ready for the controller stack install; the bootstrap can run now")
	}
	ready := true
	if domain, defaulted := spec.TrustDomain(); defaulted {
		fmt.Fprintf(out, "spire.trustDomain: not set; a bootstrap with the default cluster name and the controller stack install both use %s\n", domain)
	}
	if left := spec.ControllerStackPlaceholders(); len(left) > 0 {
		ready = false
		fmt.Fprintf(out, "controllerStack: %d values still to fill in: %s\n", len(left), strings.Join(left, ", "))
	}
	for _, check := range []func() error{spec.ValidateControllerStackValues, spec.ValidateControllerStackReferences} {
		if err := check(); err != nil {
			ready = false
			fmt.Fprintf(out, "controllerStack: %v\n", err)
		}
	}
	if !ready {
		return errors.New("the site spec is not ready for the controller stack install; the bootstrap can run now")
	}
	fmt.Fprintln(out, "controllerStack: valid")
	return nil
}
