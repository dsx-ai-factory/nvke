// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package command holds the nkectl subcommands that work with infrastructure
// providers.
//
// It sits above the registry and the inspector so that neither has to know
// about cobra, and so that a request builder depending on the registry does not
// pull in a command tree.
package command

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/config"
	"github.com/NVIDIA/nke/cli/internal/providers/inspect"
	"github.com/NVIDIA/nke/cli/internal/util"
)

// SiteFn is a factory that resolves the active site's configuration.
//
// Commands take this rather than a resolved config so that the site is only
// looked up when a command actually runs, leaving --help working on a machine
// with no site configured at all.
type SiteFn func() (config.SiteConfig, error)

// NewGetCmd returns the "nkectl get providers" subcommand.
func NewGetCmd(siteFn SiteFn, runner inspect.Runner) *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:     "providers",
		Aliases: []string{"provider"},
		Short:   "List infrastructure providers installed on the site controller",
		Long: "List infrastructure providers installed on the site controller.\n\n" +
			"This reads the site's Kubernetes cluster directly rather than the CWE API, so it " +
			"needs the kubeconfig registered with 'nkectl config set-site --kubeconfig' and " +
			"kubectl on PATH.\n\n" +
			"CONFIGURED means the provider's CRDs are established, its manager is available, and " +
			"its credentials Secret exists. It does not mean those credentials work.\n\n" +
			"The set of providers listed is the set nkectl knows how to describe. It is not a " +
			"restriction: CWE accepts any infrastructure manifest written by hand.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			site, err := siteFn()
			if err != nil {
				return err
			}
			statuses, err := inspect.Inspect(cmd.Context(), runner, site.Kubeconfig)
			if err != nil {
				return err
			}
			if f != util.FormatTable {
				return util.Print(cmd.OutOrStdout(), f, statuses)
			}
			printProviders(cmd.OutOrStdout(), statuses)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	return cmd
}

// printProviders writes a tabular summary of provider states to out.
func printProviders(out io.Writer, statuses []inspect.Status) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tSTATE\tREPLICAS\tDETAIL")
	for _, s := range statuses {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Type, s.State, replicas(s), s.Detail)
	}
	w.Flush()
}

// replicas renders the manager's readiness, or a dash when no manager was found
// to report on.
func replicas(s inspect.Status) string {
	if s.DesiredReplicas == 0 && s.ReadyReplicas == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", s.ReadyReplicas, s.DesiredReplicas)
}
