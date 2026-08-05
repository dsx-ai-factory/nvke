// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestSetUpdateInProgress(t *testing.T) {
	c := db.Cluster{}
	started := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	db.SetUpdateInProgress(&c, db.OperationTypeEtcdEncryption, "enabling", started)

	require.NotNil(t, c.Conditions.UpdateInProgress)
	assert.Equal(t, db.OperationTypeEtcdEncryption, c.Conditions.UpdateInProgress.Type)
	assert.Equal(t, "enabling", c.Conditions.UpdateInProgress.Message)
	assert.True(t, c.Conditions.UpdateInProgress.StartedAt.Equal(started))
}

func TestSetUpdateInProgress_OverwritesExisting(t *testing.T) {
	c := db.Cluster{
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{
				Type: db.OperationTypeK8sUpgrade,
			},
		},
	}
	now := time.Now().UTC()
	db.SetUpdateInProgress(&c, db.OperationTypeEtcdEncryption, "", now)

	require.NotNil(t, c.Conditions.UpdateInProgress)
	assert.Equal(t, db.OperationTypeEtcdEncryption, c.Conditions.UpdateInProgress.Type,
		"SetUpdateInProgress must replace the prior entry — callers gate on mutual exclusion before calling this")
}

func TestClearUpdateInProgress(t *testing.T) {
	c := db.Cluster{
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeK8sUpgrade},
		},
	}

	db.ClearUpdateInProgress(&c)
	assert.Nil(t, c.Conditions.UpdateInProgress)
}

func TestClearUpdateInProgress_AlreadyNil(t *testing.T) {
	c := db.Cluster{}
	db.ClearUpdateInProgress(&c)
	assert.Nil(t, c.Conditions.UpdateInProgress)
}
