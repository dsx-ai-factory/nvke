// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPruneRetainsActiveRequestAndRecentResults(t *testing.T) {
	store := NewStore(t.TempDir())
	for index := range 3 {
		request := Operation{TargetVersion: fmt.Sprintf("v0.1.%d", index), Method: MethodAPT}
		_, _, err := store.Create(request)
		require.NoError(t, err)
		_, err = store.SetStatus(request.Key(), PhaseSucceeded, "verified", "")
		require.NoError(t, err)
		dir, err := store.OperationDir(request.Key())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "retained.deb"), []byte("restore"), 0o600))
	}
	for index := range 8 {
		request := Operation{TargetVersion: fmt.Sprintf("v0.2.%d", index), Method: MethodAPT}
		_, _, err := store.Create(request)
		require.NoError(t, err)
		_, err = store.SetStatus(request.Key(), PhaseFailedRestored, "restored", "artifact missing")
		require.NoError(t, err)
	}
	_, _, err := store.Create(Operation{TargetVersion: "v0.3.0", Method: MethodBundle})
	require.NoError(t, err)
	unlock, err := store.Lock()
	require.NoError(t, err)
	defer unlock()
	require.NoError(t, store.Prune())
	operations, err := store.operations()
	require.NoError(t, err)
	require.Len(t, operations, 3)
	for _, key := range []string{"v0.1.2_apt", "v0.2.7_apt", "v0.3.0_bundle"} {
		_, err := store.Load(key)
		require.NoError(t, err)
	}
	dir, err := store.OperationDir("v0.1.2_apt")
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "retained.deb"))
	require.NoError(t, err)
	require.Equal(t, "restore", string(data))
	_, err = store.Load("v0.1.1_apt")
	require.ErrorIs(t, err, os.ErrNotExist)
}
