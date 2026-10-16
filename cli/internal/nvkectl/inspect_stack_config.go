// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
)

// newInspectStackConfigCmd validates the stack install's inputs, the stack
// config file and the credentials in the environment, and prints the few
// facts the install scripts need as shell assignments, so the shell never
// parses the file itself.
func newInspectStackConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack-config FILE",
		Short: "Validate a stack config file and the install's credentials, and print the facts the install scripts read, as shell assignments",
		Long: `Read a stack config file the way the render does: strict JSON, no REPLACE_*
placeholder left, every required value set and well-formed. Then check the
credentials the install reads from the environment: NVKECTL_NICO_TOKEN,
NVKECTL_NODE_USER, NVKECTL_NODE_PASSWORD and NVKECTL_DATABASE_PASSWORD must
be set, and the first three must hold no quote, backslash or control
character. On success print STACK_CONFIG, STACK_NICO_ENDPOINT and
STACK_NICO_ORG_ID, one per line, each single-quoted for eval; the path is
absolute. On a problem print nothing on standard output and exit non-zero,
naming every problem on standard error, the file's as "<key>: <problem>" and
the environment's as "environment: <variable> ...".`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := stackconfig.Load(args[0])
			if err != nil {
				return fmt.Errorf("stack config %s:\n%w", args[0], err)
			}
			if _, err := stackconfig.CredentialsFromEnv(); err != nil {
				lines := strings.Split(err.Error(), "\n")
				for i, line := range lines {
					lines[i] = "environment: " + line
				}
				return fmt.Errorf("stack config %s:\n%w", args[0], errors.New(strings.Join(lines, "\n")))
			}
			for _, kv := range [][2]string{
				{"STACK_CONFIG", cfg.Path()},
				{"STACK_NICO_ENDPOINT", cfg.Nico.Endpoint},
				{"STACK_NICO_ORG_ID", cfg.Nico.OrgID},
			} {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", kv[0], shellQuote(kv[1]))
			}
			return nil
		},
	}
	return cmd
}
