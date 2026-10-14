// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// newInspectSiteSpecCmd prints the few site facts the install stages need as
// shell assignments, so the shell never parses the site spec itself.
func newInspectSiteSpecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sitespec FILE",
		Short: "Print the site facts the install stages read, as shell assignments",
		Long: `Read a site spec and print SITE_NAME, SITE_VIP, SITE_TRUST_DOMAIN and
SITE_ARCH, one per line, each single-quoted for eval. SITE_VIP is the
control-plane VIP, or the only node's address. SITE_TRUST_DOMAIN is
spire.trustDomain, or the default the bootstrap and the stack install share
when the spec does not set it.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := sitespec.Load(args[0])
			if err != nil {
				return err
			}
			vip, err := spec.ControllerEndpoint()
			if err != nil {
				return err
			}
			arch := ""
			if spec.ControllerStack != nil {
				arch = spec.ControllerStack.Arch
			}
			trustDomain, _ := spec.TrustDomain()
			for _, kv := range [][2]string{{"SITE_NAME", spec.ClusterName}, {"SITE_VIP", vip}, {"SITE_TRUST_DOMAIN", trustDomain}, {"SITE_ARCH", arch}} {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", kv[0], shellQuote(kv[1]))
			}
			return nil
		},
	}
	return cmd
}
