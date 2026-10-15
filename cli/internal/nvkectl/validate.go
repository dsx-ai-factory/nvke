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
	disableMTLS   bool
}

func newValidateSiteSpecCmd() *cobra.Command {
	opts := &validateSiteSpecOptions{}
	cmd := &cobra.Command{
		Use:   "sitespec",
		Short: "Check a site spec for the bootstrap and the controller stack install",
		Long: `Check the whole site spec and report by section.

The bootstrap section is what "nvkectl bootstrap site" reads: every field is
checked, and the SPIRE CA files must exist. With infra.provider nico, the
files infra.nico names and the file NVKECTL_NICO_CREDENTIALS names must exist
on the bootstrap host, and spire.externalAddress is required unless
--disable-mtls is given, as it is for "nvkectl bootstrap site". The
controllerStack section is
what the controller stack install reads: the REPLACE_* values still to fill
in are listed apart from errors, every required field must be set, the
architecture and the NICo endpoint must be well-formed, and the files the
section names must exist.

The command exits 0 when the controllerStack section is ready, which is what
the controller stack install gates on. What only the bootstrap host holds, that
is NVKECTL_NICO_CREDENTIALS, the files infra.nico names and
spire.externalAddress, is reported but does not fail that run: the install
host does not have them. --bootstrap-only checks the bootstrap section alone,
and fails on those, for a file whose controllerStack section is not filled in
yet.`,
		Example: `  nvkectl validate sitespec -f sitespec.yaml
  nvkectl validate sitespec -f sitespec.yaml --bootstrap-only
  nvkectl validate sitespec -f sitespec.yaml --disable-mtls`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidateSiteSpec(cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "Path to the site spec (required)")
	_ = cmd.MarkFlagRequired("sitespec")
	cmd.Flags().BoolVar(&opts.bootstrapOnly, "bootstrap-only", false, "Check only the fields bootstrap site reads")
	cmd.Flags().BoolVar(&opts.disableMTLS, "disable-mtls", false,
		"Check the file as \"bootstrap site --disable-mtls\" would: with infra.provider nico, spire.externalAddress is not required")
	return cmd
}

func runValidateSiteSpec(out io.Writer, opts *validateSiteSpecOptions) error {
	spec, err := sitespec.Load(opts.siteSpec)
	if err != nil {
		return err
	}
	// The same checks bootstrap site applies before its dry-run return, so a file
	// this command passes with --bootstrap-only is one that command accepts. They
	// read what only the bootstrap host holds: the NICo token, the tenant
	// password and the release manifests, and the SPIRE address the nodes dial.
	// The controller stack install runs this command without them, so a full
	// check reports them and judges the file by the controllerStack section.
	var bootstrapErr error
	var lifts string
	if err := spec.CheckBootstrapMode(opts.disableMTLS); err != nil {
		bootstrapErr, lifts = err, " (--disable-mtls lifts this)"
	} else if err := spec.ValidateBootstrapReferences(); err != nil {
		bootstrapErr = err
	}
	switch {
	case bootstrapErr != nil && opts.bootstrapOnly:
		fmt.Fprintf(out, "bootstrap: %v%s\n", bootstrapErr, lifts)
		return errors.New("the site spec is not ready for nvkectl bootstrap site")
	case bootstrapErr != nil:
		fmt.Fprintf(out, "bootstrap: not ready for nvkectl bootstrap site on this host: %v%s "+
			"(not part of the controller stack verdict below; --bootstrap-only fails on it)\n", bootstrapErr, lifts)
	default:
		fmt.Fprintln(out, "bootstrap: valid; nvkectl bootstrap site can run with this file")
	}
	if opts.bootstrapOnly {
		return nil
	}

	// What the failure adds to the controller stack verdict: only a spec whose
	// bootstrap check passed can say the bootstrap can run.
	notReady := "the site spec is not ready for the controller stack install"
	if bootstrapErr == nil {
		notReady += "; the bootstrap can run now"
	}
	if spec.ControllerStack == nil {
		fmt.Fprintln(out, "controllerStack: missing; nvkectl generate sitespec writes the section to fill in")
		return errors.New(notReady)
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
		return errors.New(notReady)
	}
	fmt.Fprintln(out, "controllerStack: valid")
	return nil
}
