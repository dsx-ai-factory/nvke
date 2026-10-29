// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
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
	siteSpec                    string
	bootstrapOnly               bool
	disableMTLS                 bool
	allowControlPlaneWithoutVIP bool
}

func newValidateSiteSpecCmd() *cobra.Command {
	opts := &validateSiteSpecOptions{}
	cmd := &cobra.Command{
		Use:   "sitespec",
		Short: "Check a site spec the way nvkectl bootstrap site reads it",
		Long: `Check a site spec: every field is validated. With spire.upstreamCA: true,
NVKECTL_SPIRE_CA_CERT_FILE and NVKECTL_SPIRE_CA_KEY_FILE must both name
readable files; without it, they must be unset. The command prints "valid"
and exits 0 when the file is valid, and reports every problem otherwise. It
says so when spire.trustDomain is not set, since the bootstrap and the
controller stack install then both use the default.

With infra.provider nico, the bootstrap also reads what only its host holds:
the file NVKECTL_NICO_CREDENTIALS names, the files infra.nico names, the node
login in NVKECTL_NODE_USER and NVKECTL_NODE_PASSWORD, spire.externalAddress
(required unless --disable-mtls is given, as it is for "nvkectl bootstrap
site") and the https check on infra.nico.remote.endpoint. Those are reported
but do not fail the run, because the controller stack install runs this
command on a host that does not have them. --bootstrap-only fails on them, so
a file it passes is one "nvkectl bootstrap site" accepts on this host.`,
		Example: `  nvkectl validate sitespec -f sitespec.yaml
  nvkectl validate sitespec -f sitespec.yaml --bootstrap-only
  nvkectl validate sitespec -f sitespec.yaml --bootstrap-only --disable-mtls`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidateSiteSpec(cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "Path to the site spec (required)")
	_ = cmd.MarkFlagRequired("sitespec")
	cmd.Flags().BoolVar(&opts.bootstrapOnly, "bootstrap-only", false,
		"Also fail on what only the bootstrap host holds, as \"bootstrap site\" does")
	cmd.Flags().BoolVar(&opts.allowControlPlaneWithoutVIP, "allow-control-plane-without-vip", false,
		"Permit a multi-node control plane without a VIP. Development only; bootstrap site needs the same flag.")
	cmd.Flags().BoolVar(&opts.disableMTLS, "disable-mtls", false,
		"Check the file as \"bootstrap site --disable-mtls\" would: with infra.provider nico, spire.externalAddress is not required")
	return cmd
}

func runValidateSiteSpec(out io.Writer, opts *validateSiteSpecOptions) error {
	spec, err := sitespec.Load(opts.siteSpec, sitespec.WithAllowControlPlaneWithoutVIP(opts.allowControlPlaneWithoutVIP))
	if err != nil {
		return err
	}
	// The checks bootstrap site applies before its dry-run return, so a file
	// this command passes with --bootstrap-only is one that command accepts.
	// They read what only the bootstrap host holds; the controller stack
	// install runs this command without it, so without --bootstrap-only they
	// are reported and do not fail the run. Both run, so one pass reports every
	// problem.
	modeErr := spec.CheckBootstrapMode(opts.disableMTLS)
	if modeErr != nil {
		modeErr = fmt.Errorf("%w (--disable-mtls lifts this)", modeErr)
	}
	bootstrapErr := errors.Join(modeErr, spec.ValidateBootstrapReferences())
	// A failing command writes nothing to stdout: the reasons are the error,
	// which goes to stderr.
	if bootstrapErr != nil && opts.bootstrapOnly {
		return fmt.Errorf("the site spec is not ready for nvkectl bootstrap site: %w", bootstrapErr)
	}
	if bootstrapErr != nil {
		fmt.Fprintf(out, "bootstrap: not ready for nvkectl bootstrap site on this host "+
			"(--bootstrap-only fails on this): %v\n", bootstrapErr)
	}
	if domain, defaulted := spec.TrustDomain(); defaulted {
		fmt.Fprintf(out, "spire.trustDomain: not set; a bootstrap with the default cluster name and the controller stack install both use %s\n", domain)
	}
	fmt.Fprintln(out, "valid")
	return nil
}
