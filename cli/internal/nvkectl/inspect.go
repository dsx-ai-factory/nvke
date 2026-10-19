// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"strings"

	"github.com/spf13/cobra"
)

// newInspectCmd groups internal commands: hidden from the top-level help,
// text output, and no registry or cluster access. The install scripts read
// the site spec, the stack config, the stack artifacts file and values
// files through them; the verb-noun shape is the one every nvkectl command
// has.
func newInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "inspect",
		Short:  "Internal diagnostics: print one view of a file nvkectl reads",
		Hidden: true,
		Long: `Internal commands. These subcommands read a file nvkectl consumes and
print one view of it as plain text: the site spec, the stack config, the
stack artifacts file and a Helm values file. deploy/check-site.sh and
deploy/nvke-deploy.sh read them through these; operators are not expected
to. Nothing here touches a registry or a cluster.`,
	}
	cmd.AddCommand(newInspectStackArtifactsCmd(), newInspectSiteSpecCmd(), newInspectStackConfigCmd(), newInspectValuesCmd())
	return cmd
}

// shellQuote single-quotes a value for eval. Every value is quoted, even a
// safe one.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
