// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClusterConditions_UpdateInProgress_RoundTrip(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	in := db.ClusterConditions{
		UpdateInProgress: &db.UpdateInProgress{
			Type:      db.OperationTypeEtcdEncryption,
			Message:   "enabling etcd encryption",
			StartedAt: now,
		},
	}

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"updateInProgress"`)
	assert.Contains(t, string(raw), `"etcd_encryption"`)
	assert.Contains(t, string(raw), `"startedAt"`)

	var out db.ClusterConditions
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.UpdateInProgress)
	assert.Equal(t, db.OperationTypeEtcdEncryption, out.UpdateInProgress.Type)
	assert.Equal(t, "enabling etcd encryption", out.UpdateInProgress.Message)
	assert.True(t, out.UpdateInProgress.StartedAt.Equal(now))
}

func TestClusterConditions_UpdateInProgress_NilOmitEmpty(t *testing.T) {
	raw, err := json.Marshal(db.ClusterConditions{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "updateInProgress")
	assert.NotContains(t, string(raw), "updateFailed")
}

func TestClusterConditions_UpdateFailed_RoundTrip(t *testing.T) {
	updateFailed := db.NewCondition(true, "Etcd encryption update failed")
	in := db.ClusterConditions{
		UpdateFailed: &updateFailed,
	}

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"updateFailed"`)
	assert.Contains(t, string(raw), "Etcd encryption update failed")

	var out db.ClusterConditions
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.UpdateFailed)
	assert.Equal(t, db.ConditionStatusTrue, out.UpdateFailed.Status)
	assert.Equal(t, "Etcd encryption update failed", out.UpdateFailed.Message)
}

func TestOperationType_Values(t *testing.T) {
	// Guard against accidental renaming — these string values are persisted
	// in the clusters.conditions JSONB and consumed by the UI / nvkectl.
	assert.Equal(t, "etcd_encryption", string(db.OperationTypeEtcdEncryption))
	assert.Equal(t, "k8s_upgrade", string(db.OperationTypeK8sUpgrade))
	assert.Equal(t, "cp_nodes_recycling", string(db.OperationTypeCPNodesRecycling))
}
