// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clidocs

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// indexIntro is the reference's framing: what the pages are, what they leave
// out, and where the things that are not command metadata are written down.
//
// The command list below it is generated from the tree. This part is not, and
// is deliberately short: anything said here about a particular command would
// be a second copy of that command's own help text.
//
// The relative links resolve from cli/docs/reference/, in GitHub and in a
// local checkout alike. hack/docs-check.sh checks that every one of them
// points at a file that exists.
const indexIntro = `# nvkectl command reference

Every public ` + "`nvkectl`" + ` command and subcommand, generated from the command tree
the binary runs. Each page states the command's purpose, how it is invoked and
what positional arguments it takes, its aliases, its examples, and its own and
inherited options with their types and defaults.

This is the complete reference. It is not a guide: it says what each command
accepts, not when to reach for it.

## Concepts and guides

The reference covers command metadata. Everything that is not command
metadata is written down here instead.

| Document | Contents |
|---|---|
| [nvkectl README](../../README.md) | Installing nvkectl, authenticating to the management plane, and every ` + "`NVKECTL_*`" + ` environment variable |
| [Compatibility and deprecation policy](../compatibility.md) | What is contract and what is not, the exit codes, and how a command or flag is removed |
| [Site spec reference](../sitespec.md) | Every field of the site spec file ` + "`bootstrap site`" + ` reads, with its type |
| [Site and tenant cluster flow](../user-flow.md) | The commands in the order an operator runs them |
| [Bootstrap troubleshooting](../bootstrap-troubleshooting.md) | Where a failed bootstrap leaves its evidence, and the common failures |
| [Management cluster bootstrap](../../../docs/site-bootstrap/overview.md) | What a bootstrap does, end to end |
| [Authenticating to the NVKE API](../../../controller/docs/authenticating-to-the-api.md) | How a client proves who it is |
| [Configuring NVKE API authentication](../../../controller/docs/configuring-api-authentication.md) | Turning authentication on, and the RBAC nvkectl needs |

## What is not here

Hidden commands, and help topics that are prose rather than commands. A hidden
command is absent from ` + "`--help`" + ` and is not part of the public CLI, so it is
absent here for the same reason.

Everything else the binary exposes is covered, including the commands cobra
provides rather than nvkectl: ` + "`help`" + ` and ` + "`completion`" + `. A deprecated command
keeps its page, carrying its deprecation notice, for as long as it still runs.

## Commands

`

// renderIndex returns the reference's entry point: the framing above, then
// every command as a nested list, parents before children, in the order cobra
// itself lists them.
func renderIndex(root *cobra.Command) string {
	var b strings.Builder
	b.WriteString(banner)
	b.WriteString(indexIntro)
	writeEntry(&b, root, 0)
	return b.String()
}

// writeEntry writes one command's row and then its children's, indented under
// it.
func writeEntry(b *strings.Builder, cmd *cobra.Command, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s- [`%s`](%s)", indent, cmd.CommandPath(), pageName(cmd))
	if cmd.Short != "" {
		fmt.Fprintf(b, " — %s", cmd.Short)
	}
	if cmd.Deprecated != "" {
		b.WriteString(" **(deprecated)**")
	}
	b.WriteString("\n")

	for _, child := range cmd.Commands() {
		if child.Hidden || child.IsAdditionalHelpTopicCommand() {
			continue
		}
		writeEntry(b, child, depth+1)
	}
}
