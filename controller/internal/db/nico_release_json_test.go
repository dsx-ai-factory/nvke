// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestNicoProviderDecodesEitherReleaseKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "release key", input: `{"release":"nke-260318"}`, want: "nke-260318"},
		{name: "legacy os_profile key", input: `{"os_profile":"nke-260318"}`, want: "nke-260318"},
		{name: "both keys prefer release", input: `{"release":"nke-260318","os_profile":"nke-250101"}`, want: "nke-260318"},
		{name: "empty release falls back to os_profile", input: `{"release":"","os_profile":"nke-260318"}`, want: "nke-260318"},
		{name: "neither key", input: `{}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nodePool db.NicoNodePoolProvider
			require.NoError(t, json.Unmarshal([]byte(tt.input), &nodePool))
			require.Equal(t, tt.want, nodePool.Release)

			var controlPlane db.NicoControlPlaneProvider
			require.NoError(t, json.Unmarshal([]byte(tt.input), &controlPlane))
			require.Equal(t, tt.want, controlPlane.Release)
		})
	}
}

// A CWE predating the rename rewrites the whole nico payload from its own
// struct, so both keys are written to keep it reading the value during an
// upgrade and after a rollback.
func TestNicoProviderEncodesBothReleaseKeys(t *testing.T) {
	nodePool, err := json.Marshal(db.NicoNodePoolProvider{Release: "nke-260318"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"release": "nke-260318", "os_profile": "nke-260318"}, releaseKeys(t, nodePool))

	controlPlane, err := json.Marshal(db.NicoControlPlaneProvider{Release: "nke-260318"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"release": "nke-260318", "os_profile": "nke-260318"}, releaseKeys(t, controlPlane))
}

func TestNicoProviderOmitsBothReleaseKeysWhenUnset(t *testing.T) {
	nodePool, err := json.Marshal(db.NicoNodePoolProvider{ConfigProfile: "gpu"})
	require.NoError(t, err)
	require.Empty(t, releaseKeys(t, nodePool))

	controlPlane, err := json.Marshal(db.NicoControlPlaneProvider{ConfigProfile: "gpu"})
	require.NoError(t, err)
	require.Empty(t, releaseKeys(t, controlPlane))
}

func TestNicoProviderRoundTripsLegacyPayload(t *testing.T) {
	var nodePool db.NicoNodePoolProvider
	require.NoError(t, json.Unmarshal([]byte(`{"os_profile":"nke-260318","config_profile":"gpu"}`), &nodePool))

	encoded, err := json.Marshal(nodePool)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"release": "nke-260318", "os_profile": "nke-260318"}, releaseKeys(t, encoded))
}

// Inserts go through bun rather than json.Marshal directly, so the mirror is
// only worth anything if bun's jsonb encoding honors MarshalJSON.
func TestNicoProviderWritesBothKeysOnInsert(t *testing.T) {
	ctx := t.Context()

	t.Run("node pool", func(t *testing.T) {
		bunDB := testutil.SetupDB(ctx, t, (*db.NodePool)(nil))
		pool := &db.NodePool{ClusterID: uuid.New(), Name: "pool"}
		pool.SetNico(db.NicoNodePoolProvider{Release: "nke-260318"})
		_, err := bunDB.NewInsert().Model(pool).Exec(ctx)
		require.NoError(t, err)

		require.Equal(t, map[string]string{"release": "nke-260318", "os_profile": "nke-260318"},
			releaseKeys(t, storedJSON(ctx, t, bunDB, "node_pools", "nico")))
	})

	t.Run("node set", func(t *testing.T) {
		bunDB := testutil.SetupDB(ctx, t, (*db.NodeSet)(nil))
		nodeSet := &db.NodeSet{NodePoolID: uuid.New()}
		nodeSet.SetNico(db.NicoNodePoolProvider{Release: "nke-260318"})
		_, err := bunDB.NewInsert().Model(nodeSet).Exec(ctx)
		require.NoError(t, err)

		require.Equal(t, map[string]string{"release": "nke-260318", "os_profile": "nke-260318"},
			releaseKeys(t, storedJSON(ctx, t, bunDB, "node_sets", "nico")))
	})

	t.Run("control plane configuration", func(t *testing.T) {
		bunDB := testutil.SetupDB(ctx, t, (*db.ControlPlaneConfiguration)(nil))
		spec := db.ControlPlaneConfigSpec{}
		spec.SetNico(db.NicoControlPlaneProvider{Release: "nke-260318"})
		_, err := bunDB.NewInsert().Model(&db.ControlPlaneConfiguration{ClusterID: uuid.New(), Config: spec}).Exec(ctx)
		require.NoError(t, err)

		var nested struct {
			Nico json.RawMessage `json:"nico"`
		}
		require.NoError(t, json.Unmarshal(storedJSON(ctx, t, bunDB, "control_plane_configurations", "config"), &nested))
		require.Equal(t, map[string]string{"release": "nke-260318", "os_profile": "nke-260318"}, releaseKeys(t, nested.Nico))
	})
}

// storedJSON reads a JSON column back as raw bytes, bypassing the decode hook.
func storedJSON(ctx context.Context, t *testing.T, bunDB *bun.DB, table, column string) []byte {
	t.Helper()

	var stored string
	require.NoError(t, bunDB.NewSelect().Table(table).Column(column).Limit(1).Scan(ctx, &stored))
	return []byte(stored)
}

// releaseKeys returns whichever of the two release keys the payload carries.
func releaseKeys(t *testing.T, payload []byte) map[string]string {
	t.Helper()

	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &decoded))

	keys := map[string]string{}
	for _, key := range []string{"release", "os_profile"} {
		if raw, ok := decoded[key]; ok {
			var value string
			require.NoError(t, json.Unmarshal(raw, &value))
			keys[key] = value
		}
	}
	return keys
}
