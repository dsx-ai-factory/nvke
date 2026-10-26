// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package command holds the nvkectl subcommands that work with infrastructure
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
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/providers/credentials"
	"github.com/dsx-ai-factory/nvke/cli/internal/providers/inspect"
	"github.com/dsx-ai-factory/nvke/cli/internal/util"
)

// SiteFn is a factory that resolves the active site's configuration.
//
// Commands take this rather than a resolved config so that the site is only
// looked up when a command actually runs, leaving --help working on a machine
// with no site configured at all.
type SiteFn func() (config.SiteConfig, error)

// NewGetCmd returns the "nvkectl get providers" subcommand. newLister builds
// the client that reads providers' Identities from the site's cluster.
func NewGetCmd(siteFn SiteFn, runner inspect.Runner, newLister credentials.ListerFactory) *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:     "providers",
		Aliases: []string{"provider"},
		Short:   "List infrastructure providers installed on the management cluster",
		Long: "List infrastructure providers installed on the management cluster.\n\n" +
			"This reads the site's Kubernetes cluster directly rather than the controller API, so it " +
			"needs the kubeconfig registered with 'nvkectl config set-site --kubeconfig' and " +
			"kubectl on PATH.\n\n" +
			"CONFIGURED means the provider's CRDs are established, its manager is available, and " +
			"its credentials Secret exists. It does not mean those credentials work.\n\n" +
			"CREDENTIALS is the provider's own credential check, read from the NicoIdentity that " +
			"names the provider's credentials Secret. nvkectl never runs a check: the provider " +
			"checks every few minutes, and LAST CHECK is how long ago the latest check ran. A result " +
			"is STALE when the provider's scheduled check is overdue or, if the provider publishes no " +
			"usable schedule, when its last check, or its creation if it was never checked, is too old.\n\n" +
			"  HEALTHY            the last check authenticated and read the current tenant\n" +
			"  FAILING (reason)   the credentials are missing, invalid, rejected or forbidden\n" +
			"  UNKNOWN (reason)   the check could not decide, for example during an outage\n" +
			"  STALE              no check when one was due\n" +
			"  PENDING            not checked yet\n" +
			"  NO-IDENTITY        no NicoIdentity names the provider's credentials Secret\n" +
			"  UNSUPPORTED        the provider does not serve NicoIdentity\n" +
			"  UNREADABLE         nvkectl could not list NicoIdentities\n" +
			"  -                  the provider has no credential health to report\n\n" +
			"HEALTHY does not prove that provisioning will succeed.\n\n" +
			"The set of providers listed is the set nvkectl knows how to describe. It is not a " +
			"restriction: the controller accepts any infrastructure manifest written by hand.\n\n" +
			"Alpha: this command's output may change between releases.",
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
			statuses, err := inspect.Inspect(cmd.Context(), runner, newLister, site.Kubeconfig)
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
	fmt.Fprintln(w, "PROVIDER\tSTATE\tREPLICAS\tCREDENTIALS\tLAST CHECK\tDETAIL")
	for _, s := range statuses {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Type, s.State, replicas(s), s.Credentials.Cell(), lastCheck(s.Credentials), detail(s))
	}
	w.Flush()
}

// lastCheck renders how long ago the provider's last reported credential
// check ran, or a dash when there is no trustworthy check time.
func lastCheck(c *credentials.Credentials) string {
	if c == nil || c.LastCheckedTime == nil || (c.Health == credentials.HealthUnknown && c.LastCheckedTime.After(time.Now())) {
		return "-"
	}
	return util.FormatAge(c.LastCheckedTime.Time)
}

// detail explains a provider's row: its STATE problem when there is one,
// otherwise its credential health.
func detail(s inspect.Status) string {
	if s.Detail != "" || s.Credentials == nil {
		return s.Detail
	}
	return s.Credentials.Detail
}

// replicas renders the manager's readiness, or a dash when no manager was found
// to report on.
func replicas(s inspect.Status) string {
	if s.DesiredReplicas == 0 && s.ReadyReplicas == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", s.ReadyReplicas, s.DesiredReplicas)
}
