// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestOpenVSwitchConfigPersistence(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.NodePool)(nil), (*db.NodeSet)(nil), (*db.ControlPlaneConfiguration)(nil))
	want := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	for _, config := range []*db.OpenVSwitchConfig{nil, want} {
		pool := &db.NodePool{ID: uuid.New(), ClusterID: uuid.New(), Name: "workers", NodeNamePrefix: "w", NodeLabels: map[string]string{}, OpenVSwitch: config}
		_, err := bunDB.NewInsert().Model(pool).Exec(ctx)
		require.NoError(t, err)
		storedPool := &db.NodePool{ID: pool.ID}
		require.NoError(t, bunDB.NewSelect().Model(storedPool).WherePK().Scan(ctx))
		assert.Equal(t, config, storedPool.OpenVSwitch)
		set := &db.NodeSet{ID: uuid.New(), NodePoolID: pool.ID, OpenVSwitch: config}
		_, err = bunDB.NewInsert().Model(set).Exec(ctx)
		require.NoError(t, err)
		storedSet := &db.NodeSet{ID: set.ID}
		require.NoError(t, bunDB.NewSelect().Model(storedSet).WherePK().Scan(ctx))
		assert.Equal(t, config, storedSet.OpenVSwitch)
		row := &db.ControlPlaneConfiguration{ID: uuid.New(), ClusterID: pool.ClusterID, Config: db.ControlPlaneConfigSpec{OpenVSwitch: config}}
		_, err = bunDB.NewInsert().Model(row).Exec(ctx)
		require.NoError(t, err)
		storedRow := &db.ControlPlaneConfiguration{ID: row.ID}
		require.NoError(t, bunDB.NewSelect().Model(storedRow).WherePK().Scan(ctx))
		assert.Equal(t, config, storedRow.Config.OpenVSwitch)
	}
}

func TestOpenVSwitchControlPlaneRollout(t *testing.T) {
	legacy := db.ControlPlaneConfigSpec{}
	explicit := db.ControlPlaneConfigSpec{OpenVSwitch: &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}}
	assert.False(t, db.IsControlPlaneRolloutRequired(legacy, legacy))
	assert.True(t, db.IsControlPlaneRolloutRequired(legacy, explicit))
	assert.True(t, db.IsControlPlaneRolloutRequired(explicit, legacy))
	same := db.ControlPlaneConfigSpec{OpenVSwitch: explicit.OpenVSwitch.Clone()}
	assert.False(t, db.IsControlPlaneRolloutRequired(explicit, same))
	same.OpenVSwitch.BridgeName = "br-eth2"
	assert.True(t, db.IsControlPlaneRolloutRequired(explicit, same))
	assert.False(t, explicit.Equal(same))
	assert.Equal(t, "br-eth1", explicit.OpenVSwitch.BridgeName)
}

func TestOpenVSwitchLegacyControlPlaneMapping(t *testing.T) {
	pool := db.NodePool{ControlPlane: true, OpenVSwitch: &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}}
	config := db.ControlPlaneConfigFromNodePool(&pool)
	require.NotNil(t, config)
	assert.Equal(t, pool.OpenVSwitch, config.OpenVSwitch)
	pool.OpenVSwitch.BridgeName = "br-eth2"
	assert.Equal(t, "br-eth1", config.OpenVSwitch.BridgeName)
}
