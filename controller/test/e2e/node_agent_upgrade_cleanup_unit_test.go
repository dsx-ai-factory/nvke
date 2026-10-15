// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

func TestUnit_AgentUpgradeCleanupContinuesAfterContainerStops(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	fakeDocker := `#!/bin/sh
case "$1" in
  ps)
    case "$5" in
      *stopping*) printf '%s\n' stopping ;;
      *surviving*) printf '%s\n' surviving ;;
    esac
    ;;
  exec)
    printf '%s\n' "$2" >> "$AGENT_CLEANUP_TEST_CALLS"
    case "$2" in stopping) exit 1 ;; esac
    ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "docker"), []byte(fakeDocker), 0o700))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_CLEANUP_TEST_CALLS", calls)
	cleanupAgentInstallations(t.Context(), []api.Node{{Name: "stopping"}, {Name: "surviving"}})
	released, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "stopping\nsurviving\n", string(released))
}
