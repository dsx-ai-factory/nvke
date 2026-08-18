// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestNodePoolConfigChangeInput(t *testing.T) {
	nodeSetID := uuid.New()

	base := db.NodePool{
		ID:               uuid.New(),
		CurrentNodeSetID: &nodeSetID,
		OSProfile:        "nke-260602-24.04-x86-fde",
		DesiredCount:     3,
		MaxSurge:         1,
	}

	t.Run("audited field changed emits a diff", func(t *testing.T) {
		updated := base
		updated.OSProfile = "nke-260625-24.04-x86-fde"

		input, changed := nodePoolConfigChangeInput(base, updated)
		require.True(t, changed)
		assert.Equal(t, base.ID, input.NodePoolID)
		assert.Equal(t, &nodeSetID, input.PrevNodeSetID)
		assert.NotEqual(t, uuid.Nil, input.NewNodeSetID)

		diff := configDiffFromInput(input)
		require.Len(t, diff, 1)
		assert.Equal(t, "osProfile", diff[0].Field)
		require.NotNil(t, diff[0].From)
		assert.Equal(t, "nke-260602-24.04-x86-fde", *diff[0].From)
		assert.Equal(t, "nke-260625-24.04-x86-fde", diff[0].To)
	})

	t.Run("no audited change does not emit", func(t *testing.T) {
		updated := base
		_, changed := nodePoolConfigChangeInput(base, updated)
		assert.False(t, changed)
	})

	t.Run("non-audited fields alone do not emit", func(t *testing.T) {
		// desiredCount / maxSurge are scaling knobs, not config; they must not produce a record.
		updated := base
		updated.DesiredCount = 5
		updated.MaxSurge = 2

		_, changed := nodePoolConfigChangeInput(base, updated)
		assert.False(t, changed)
	})

	t.Run("without a current NodeSet the diff cannot render", func(t *testing.T) {
		noNodeSet := base
		noNodeSet.CurrentNodeSetID = nil
		updated := noNodeSet
		updated.OSProfile = "nke-260625-24.04-x86-fde"

		_, changed := nodePoolConfigChangeInput(noNodeSet, updated)
		assert.False(t, changed)
	})
}
