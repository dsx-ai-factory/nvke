// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command nvkectl is the NVKE control-plane CLI.
//
// This package holds only the build-time version symbols and the call into
// internal/nvkectl, which carries the command tree. The split is what makes the
// tree reachable from a second program: a command tree in package main can be
// executed but never imported, and the reference documentation under
// cli/docs/reference is generated from the same tree this binary runs.
package main

import (
	"os"

	"github.com/dsx-ai-factory/nvke/cli/internal/nvkectl"
)

// Build-time variables injected via ldflags.
//
// They stay in package main because the linker matches -X on the package NAME,
// and for a command that name is "main". Moved into internal/nvkectl they would
// be silently left at the defaults below -- no error, no warning. cli/Makefile
// runs the built binary and fails when it reports one of these.
var (
	version      = "dev"     //nolint:gochecknoglobals // set by ldflags at build time
	gitCommit    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	gitTreeState = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	buildDate    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
)

// Terminating the process happens here and nowhere else. nvkectl.Run returns
// the status rather than exiting, so that importing it cannot kill a program
// that has deferred work outstanding.
func main() {
	os.Exit(nvkectl.Run(version, gitCommit, gitTreeState, buildDate))
}
