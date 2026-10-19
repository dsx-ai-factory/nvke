// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The generator's tests against the real nvkectl tree. clidocs_test.go covers
// the rendering rules with a tree built for the purpose; these cover the
// properties that only mean something for the command tree actually shipped.
package clidocs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/nvkectl"
)

// generateNkectl writes the real reference into a temporary directory and
// returns the pages, keyed by file name.
func generateNkectl(t *testing.T) map[string]string {
	t.Helper()

	dir := t.TempDir()
	root := nvkectl.NewRootCmd(PinnedVersion, PinnedCommit, PinnedTreeState, PinnedBuildDate)
	require.NoError(t, Generate(root, dir))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	pages := make(map[string]string, len(entries))
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		pages[e.Name()] = string(data)
	}
	return pages
}

// TestGenerationIgnoresTheEnvironment is the determinism property the
// committed output depends on.
//
// Two of nvkectl's flag defaults are read from the environment when the command
// is constructed: bootstrap site's --diagnostics-dir and --timeout. Without
// PinEnvironment, whoever regenerated the reference last would write their own
// shell's values into it, and the next contributor's check would fail on a
// diff they did not cause.
func TestGenerationIgnoresTheEnvironment(t *testing.T) {
	// Not parallel: it sets process-wide environment variables.
	t.Setenv("NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR", "/somewhere/else")
	t.Setenv("NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT", "99m")
	t.Setenv("NVKECTL_CONTROLLER_IMAGE", "example.invalid/controller:pinned")
	t.Setenv("NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR", "/the/legacy/name")
	t.Setenv("HOME", "/home/someone-else")
	t.Setenv("XDG_CONFIG_HOME", "/home/someone-else/.config")

	PinEnvironment()
	polluted := generateNkectl(t)

	t.Setenv("NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR", "/a/third/place")
	t.Setenv("NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT", "1s")
	t.Setenv("HOME", "/home/yet-another")

	PinEnvironment()
	clean := generateNkectl(t)

	require.Equal(t, clean, polluted, "the reference must not depend on the environment it was generated in")

	// Named explicitly, so this keeps testing what it is about even if the two
	// trees ever agree for some other reason.
	page := clean["nvkectl_bootstrap_site.md"]
	require.Contains(t, page, `(default "cluster-diagnostics")`)
	require.Contains(t, page, "(default 30m0s)")
	require.NotContains(t, page, "somewhere/else")
	require.NotContains(t, page, "99m")
	require.NotContains(t, page, "the/legacy/name")
	require.NotContains(t, page, "someone-else")
}

// TestGenerationIsRepeatable is the other half: the same source, generated
// twice, produces the same bytes.
func TestGenerationIsRepeatable(t *testing.T) {
	PinEnvironment()
	require.Equal(t, generateNkectl(t), generateNkectl(t))
}

// TestPublicFrameworkCommandsAreCovered pins the commands cobra adds lazily.
//
// Neither exists until something initialises it, so a traversal that only
// walked the constructors would miss both -- and the root page links to them
// regardless, leaving two links resolving to nothing.
func TestPublicFrameworkCommandsAreCovered(t *testing.T) {
	PinEnvironment()
	pages := generateNkectl(t)

	for _, name := range []string{
		"nvkectl_help.md",
		"nvkectl_completion.md",
		"nvkectl_completion_bash.md",
		"nvkectl_completion_fish.md",
		"nvkectl_completion_powershell.md",
		"nvkectl_completion_zsh.md",
	} {
		require.Contains(t, pages, name, "the reference should cover %s", name)
	}
}

// TestNoHelpSubcommandUnderGroupCommands is the nvkectl-tree half of the
// regression covered by TestGroupCommandsGetNoHelpSubcommand: `nvkectl
// bootstrap help` is not a command, and must not be documented as one.
func TestNoHelpSubcommandUnderGroupCommands(t *testing.T) {
	PinEnvironment()
	pages := generateNkectl(t)

	for _, group := range []string{"bootstrap", "config", "create", "delete", "deprecate", "generate", "get", "patch"} {
		require.NotContains(t, pages, "nvkectl_"+group+"_help.md",
			"nvkectl %s help is not a command", group)
	}
}

// TestEveryCommandHasAPage walks the tree the generator walked and asserts
// nothing was skipped, so a command added without a page fails here.
func TestEveryCommandHasAPage(t *testing.T) {
	PinEnvironment()
	pages := generateNkectl(t)

	root := nvkectl.NewRootCmd(PinnedVersion, PinnedCommit, PinnedTreeState, PinnedBuildDate)
	prepare(root)
	for _, cmd := range commands(root) {
		require.Contains(t, pages, pageName(cmd), "%s has no page", cmd.CommandPath())
	}
}
