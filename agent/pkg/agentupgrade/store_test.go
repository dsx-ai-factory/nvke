// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoreUsesVersionAndMethodForIdempotenceAndSerialAdmission(t *testing.T) {
	store := NewStore(t.TempDir())
	request := Operation{TargetVersion: "v0.1.2", PreviousVersion: "v0.1.1", Method: MethodAPT}
	created, wasCreated, err := store.Create(request)
	require.NoError(t, err)
	require.True(t, wasCreated)
	retry := request
	retry.PreviousVersion = "v0.1.2"
	existing, wasCreated, err := store.Create(retry)
	require.NoError(t, err)
	require.False(t, wasCreated)
	require.Equal(t, created, existing)
	competing := request
	competing.Method = MethodBundle
	_, _, err = store.Create(competing)
	require.ErrorIs(t, err, ErrOperationActive)
	_, err = store.SetStatus(request.Key(), PhaseFailedRestored, "restored", "bad artifact")
	require.NoError(t, err)
	retried, wasCreated, err := store.Create(request)
	require.NoError(t, err)
	require.True(t, wasCreated)
	require.Equal(t, created.Key(), retried.Key())
	require.Equal(t, PhasePending, retried.Phase)
	require.Empty(t, retried.ErrorMessage)
	_, err = store.SetStatus(request.Key(), PhaseSucceeded, "verified", "")
	require.NoError(t, err)
	_, wasCreated, err = store.Create(competing)
	require.NoError(t, err)
	require.True(t, wasCreated)
}

func TestValidateRequestKey(t *testing.T) {
	for _, key := range []string{"v0.1.2_apt", "v0.1.2_bundle"} {
		require.NoError(t, ValidateRequestKey(key))
	}
	for _, key := range []string{"", "../escape", "v0.1.2@apt", "v01.1.2_apt", "v0.1.2_other"} {
		require.Error(t, ValidateRequestKey(key))
	}
}
