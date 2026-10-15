// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command qualify drives release qualification. `plan` resolves what to
// qualify, and `verify` checks that what a plan names is published.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is stamped at build time. Declared so the -X the Makefile passes
// lands somewhere: the linker ignores an -X for a symbol the binary lacks,
// and it does so silently.
var version = "dev"

func main() {
	if err := root().Execute(); err != nil {
		// Cobra has already printed usage errors. Anything else is ours and
		// is printed here, once.
		var silent silentError
		if !errors.As(err, &silent) {
			fmt.Fprintln(os.Stderr, "qualify:", err)
		}
		os.Exit(1)
	}
}

// silentError marks an error cobra has already reported.
type silentError struct{ error }

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "qualify",
		Short:   "Drive NVKE release qualification",
		Version: version,
		Long: `Drive NVKE release qualification.

  plan       resolve what to qualify
  verify     check that everything a plan names is published`,
		SilenceUsage: true,
	}
	cmd.AddCommand(planCmd(), verifyCmd())
	return cmd
}
