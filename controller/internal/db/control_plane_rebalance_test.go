// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// TestCappedControlPlaneConfig pins the provider cap seam: a provider with an
// ordered preference truncates it to the rebalance cap and leaves the receiver
// untouched, while a provider that expresses no ordered preference reports that
// it cannot cap.
func TestCappedControlPlaneConfig(t *testing.T) {
	t.Parallel()

	a, b, c := uuid.New(), uuid.New(), uuid.New()
	nicoSpec := func() db.ControlPlaneConfigSpec {
		var s db.ControlPlaneConfigSpec
		s.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{a, b, c}})
		return s
	}

	t.Run("truncates the Nico preference to the cap", func(t *testing.T) {
		t.Parallel()

		spec := nicoSpec()
		capped, ok := spec.CappedControlPlaneConfig(2)
		require.True(t, ok)
		got, err := capped.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, []uuid.UUID{a, b}, got.InstanceTypePreference, "the preference is capped to the first two entries")

		orig, err := spec.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, []uuid.UUID{a, b, c}, orig.InstanceTypePreference, "the receiver is not mutated")
	})

	t.Run("a cap at or beyond the list length leaves the preference unchanged", func(t *testing.T) {
		t.Parallel()

		capped, ok := nicoSpec().CappedControlPlaneConfig(3)
		require.True(t, ok)
		got, err := capped.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, []uuid.UUID{a, b, c}, got.InstanceTypePreference)
	})

	t.Run("a provider with no ordered preference cannot cap", func(t *testing.T) {
		t.Parallel()

		var capi db.ControlPlaneConfigSpec
		capi.SetCapi(db.CAPIControlPlaneProvider{})
		_, ok := capi.CappedControlPlaneConfig(1)
		assert.False(t, ok, "CAPI does not implement the capper, so rebalance is a no-op for it")
	})

	t.Run("an empty spec has no provider to cap", func(t *testing.T) {
		t.Parallel()

		_, ok := db.ControlPlaneConfigSpec{}.CappedControlPlaneConfig(1)
		assert.False(t, ok)
	})
}

// TestFindOrCreateControlPlaneConfiguration pins the transient-snapshot resolver:
// it reuses an existing row whose config matches and inserts one only when none
// does, without moving the cluster's own configuration pointer.
func TestFindOrCreateControlPlaneConfiguration(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	var spec db.ControlPlaneConfigSpec
	spec.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{uuid.New()}})

	t.Run("reuses an existing snapshot whose config matches", func(t *testing.T) {
		t.Parallel()

		existingID := uuid.New()
		m := &testutil.MockDB{}
		m.On("ListControlPlaneConfigurationsByCluster", mock.Anything, clusterID).
			Return([]db.ControlPlaneConfiguration{{ID: existingID, ClusterID: clusterID, Config: spec}}, nil)

		id, err := db.FindOrCreateControlPlaneConfiguration(context.Background(), m, clusterID, spec, "reason")
		require.NoError(t, err)
		assert.Equal(t, existingID, id)
		m.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
	})

	t.Run("creates a snapshot when none matches", func(t *testing.T) {
		t.Parallel()

		newID := uuid.New()
		m := &testutil.MockDB{}
		m.On("ListControlPlaneConfigurationsByCluster", mock.Anything, clusterID).
			Return([]db.ControlPlaneConfiguration{}, nil)
		m.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
			return row.ClusterID == clusterID && row.Config.Equal(spec) && row.ChangeReason == "reason"
		})).Return(db.ControlPlaneConfiguration{ID: newID, ClusterID: clusterID, Config: spec}, nil)

		id, err := db.FindOrCreateControlPlaneConfiguration(context.Background(), m, clusterID, spec, "reason")
		require.NoError(t, err)
		assert.Equal(t, newID, id)
	})
}
