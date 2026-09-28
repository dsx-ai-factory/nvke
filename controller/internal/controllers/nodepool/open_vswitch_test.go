// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepool

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestOpenVSwitchNodeSetSnapshot(t *testing.T) {
	pool := db.NodePool{OpenVSwitch: &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}}
	snapshot := buildBaseNodeSet(pool)
	assert.Equal(t, pool.OpenVSwitch, snapshot.OpenVSwitch)
	pool.OpenVSwitch.UplinkInterface = "eth2"
	assert.Equal(t, "eth1", snapshot.OpenVSwitch.UplinkInterface)
	assert.Nil(t, buildBaseNodeSet(db.NodePool{}).OpenVSwitch)
}

func TestOpenVSwitchChangeCreatesNodeSet(t *testing.T) {
	config := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	tests := []struct {
		name string
		old  *db.OpenVSwitchConfig
		new  *db.OpenVSwitchConfig
	}{
		{name: "enable explicit configuration", new: config},
		{name: "change explicit configuration", old: config, new: &db.OpenVSwitchConfig{UplinkInterface: "eth2", BridgeName: "br-eth2"}},
		{name: "restore automatic selection", old: config},
	}

	for _, provider := range []db.ProviderType{db.ProviderTypeNico, db.ProviderTypeCAPI} {
		for _, tt := range tests {
			t.Run(string(provider)+"/"+tt.name, func(t *testing.T) {
				oldID, newID := uuid.New(), uuid.New()
				pool := db.NodePool{ID: uuid.New(), CurrentNodeSetID: &oldID, OpenVSwitch: tt.new}
				oldSet := db.NodeSet{ID: oldID, NodePoolID: pool.ID, OpenVSwitch: tt.old}
				newSet := db.NodeSet{ID: newID, NodePoolID: pool.ID, OpenVSwitch: pool.OpenVSwitch.Clone()}
				if provider == db.ProviderTypeNico {
					pool.SetNico(db.NicoNodePoolProvider{})
					oldSet.SetNico(db.NicoNodePoolProvider{})
				} else {
					pool.SetCapi(db.CAPINodePoolProvider{})
					oldSet.SetCapi(db.CAPINodePoolProvider{})
				}
				updatedPool := pool
				updatedPool.CurrentNodeSetID = &newID
				tx := &testutil.MockTx{}
				tx.On("CreateNodeSet", mock.Anything, mock.MatchedBy(func(set *db.NodeSet) bool {
					return set.OpenVSwitch.Equal(pool.OpenVSwitch) && (pool.OpenVSwitch == nil || set.OpenVSwitch != pool.OpenVSwitch)
				})).Return(newSet, nil).Once()
				tx.On("UpdateNodePool", mock.Anything, mock.Anything).Return(updatedPool, nil).Once()
				controller := &Controller{}
				_, _, err := controller.createNodeSetIfChanged(context.Background(), tx, db.Cluster{ProviderType: provider}, pool, oldSet, slog.Default())
				require.NoError(t, err)
				tx.AssertExpectations(t)
			})
		}
	}
}
