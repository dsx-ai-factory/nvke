// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command qualify drives release qualification: it resolves what to qualify,
// runs it, records the result, and renders what is qualified.
//
// Only `plan` exists so far. The others are named in the root help rather
// than stubbed, because a command that parses its flags and then refuses is
// harder to tell from one that is merely broken.
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
		Short:   "Drive NKE release qualification",
		Version: version,
		Long: `Drive NKE release qualification.

The tool has five verbs. Only plan is implemented.

  plan       resolve what to qualify                  (implemented)
  run        acquire capacity, call the suite, release
  publish    record one stream's outcome
  aggregate  rebuild one artifact from every result
  generate   render the dashboard from it`,
		SilenceUsage: true,
	}
	cmd.AddCommand(planCmd())
	return cmd
}
