// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreCreateIsIdempotentAndSerial(t *testing.T) {
	store := NewStore(t.TempDir())
	request := Operation{
		ID:              "cluster-node-1",
		Action:          ActionUpgrade,
		TargetVersion:   "v0.1.2",
		PreviousVersion: "v0.1.1",
		Method:          MethodAPT,
	}

	created, wasCreated, err := store.Create(request)
	require.NoError(t, err)
	assert.True(t, wasCreated)
	assert.Equal(t, PhasePending, created.Phase)

	retry := request
	retry.PreviousVersion = "v0.1.2"
	existing, wasCreated, err := store.Create(retry)
	require.NoError(t, err)
	assert.False(t, wasCreated)
	assert.Equal(t, created, existing)

	_, _, err = store.Create(Operation{
		ID:              "cluster-node-2",
		Action:          ActionUpgrade,
		TargetVersion:   "v0.1.2",
		PreviousVersion: "v0.1.1",
		Method:          MethodAPT,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOperationActive)

	_, err = store.SetStatus(request.ID, PhaseSucceeded, "verified", "")
	require.NoError(t, err)
	_, wasCreated, err = store.Create(Operation{
		ID:              "cluster-node-2",
		Action:          ActionUpgrade,
		TargetVersion:   "v0.1.2",
		PreviousVersion: "v0.1.1",
		Method:          MethodAPT,
	})
	require.NoError(t, err)
	assert.True(t, wasCreated)
}

func TestStoreRejectsOperationIDReuseForDifferentRequest(t *testing.T) {
	store := NewStore(t.TempDir())
	_, _, err := store.Create(Operation{
		ID:              "same-id",
		Action:          ActionUpgrade,
		TargetVersion:   "v0.1.2",
		PreviousVersion: "v0.1.1",
		Method:          MethodAPT,
	})
	require.NoError(t, err)

	_, _, err = store.Create(Operation{
		ID:              "same-id",
		Action:          ActionUpgrade,
		TargetVersion:   "v0.1.3",
		PreviousVersion: "v0.1.1",
		Method:          MethodAPT,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrOperationConflict))
}

func TestValidateOperationID(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		wantError bool
	}{
		{name: "valid", id: "cluster.node-1_upgrade", wantError: false},
		{name: "empty", id: "", wantError: true},
		{name: "path traversal", id: "../escape", wantError: true},
		{name: "systemd escape", id: "node@upgrade", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateOperationID(test.id)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
