// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
