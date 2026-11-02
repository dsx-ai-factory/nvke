// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestNicoControlPlaneProviderScaleDownRank(t *testing.T) {
	preferred := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	lessPreferred := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	offList := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003")

	provider := db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{preferred, lessPreferred}}
	nodeWithType := func(id uuid.UUID) db.Node {
		return db.Node{Nico: &db.NicoNodeProvider{InstanceTypeID: id}}
	}

	// Most preferred ranks lowest (shed last); off-list and unplaced rank
	// highest (shed first).
	assert.Equal(t, 0, provider.ScaleDownRank(nodeWithType(preferred)))
	assert.Equal(t, 1, provider.ScaleDownRank(nodeWithType(lessPreferred)))
	assert.Equal(t, 2, provider.ScaleDownRank(nodeWithType(offList)))
	assert.Equal(t, 2, provider.ScaleDownRank(db.Node{}), "a node with no recorded type is shed first")
}

func TestControlPlaneConfigSpecScaleDownRanker(t *testing.T) {
	t.Run("nico provider is its own ranker", func(t *testing.T) {
		var spec db.ControlPlaneConfigSpec
		spec.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{uuid.New()}})

		ranker := spec.ScaleDownRanker()
		require.NotNil(t, ranker, "a nico config expresses a placement preference")
		assert.IsType(t, db.NicoControlPlaneProvider{}, ranker)
	})

	t.Run("capi provider expresses no preference yet", func(t *testing.T) {
		var spec db.ControlPlaneConfigSpec
		spec.SetCapi(db.CAPIControlPlaneProvider{})

		assert.Nil(t, spec.ScaleDownRanker(), "the CAPI/infra path ranks by age until it records placement")
	})

	t.Run("no provider set", func(t *testing.T) {
		assert.Nil(t, db.ControlPlaneConfigSpec{}.ScaleDownRanker())
	})
}
