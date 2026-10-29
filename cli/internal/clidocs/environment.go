// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clidocs

import (
	"os"
	"strings"
)

// Build metadata the reference is generated with.
//
// Fixed strings, not the real build's. No page shows a version today -- the
// version command prints one when it runs, and prints nothing about it in its
// help -- but a page that started to would otherwise change on every commit
// and make the committed output impossible to keep current.
const (
	PinnedVersion      = "(version)"
	PinnedCommit       = "(commit)"
	PinnedTreeState    = "(tree state)"
	PinnedBuildDate    = "(build date)"
	pinnedHome         = "/home/nvkectl"
	pinnedXDGConfigDir = "/home/nvkectl/.config"
)

// PinEnvironment removes the environment's influence over the command tree,
// and must be called before the tree is built.
//
// Two of nvkectl's flag defaults are read from the environment when the command
// is constructed rather than when it runs: bootstrap site's --diagnostics-dir
// takes NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR and its --timeout takes
// NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT. Generated on a machine where either is
// set, the page would record that machine's value as nvkectl's default.
//
// Every NVKECTL_ and NKECTL_ variable is cleared rather than those two, so a
// default added later is covered without anyone remembering to come back here.
// HOME and the XDG config directory are pinned for the same reason: no help
// text derives from them today, and one that started to would otherwise carry
// the generating user's home directory into the committed pages.
func PinEnvironment() {
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		// Both prefixes. internal/envalias reads NVKECTL_X first and falls
		// back to the NKECTL_X the CLI used before it was renamed, so the old
		// name still reaches a flag default and still has to be cleared.
		if strings.HasPrefix(name, "NVKECTL_") || strings.HasPrefix(name, "NKECTL_") {
			os.Unsetenv(name)
		}
	}
	os.Setenv("HOME", pinnedHome)
	os.Setenv("XDG_CONFIG_HOME", pinnedXDGConfigDir)
}
