// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
)

// Build-time variables injected via ldflags.
var (
	version      = "dev"     //nolint:gochecknoglobals // set by ldflags at build time
	gitCommit    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	gitTreeState = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
	buildDate    = "unknown" //nolint:gochecknoglobals // set by ldflags at build time
)

func main() {
	if err := Execute(version, gitCommit, gitTreeState, buildDate); err != nil {
		os.Exit(1)
	}
}
