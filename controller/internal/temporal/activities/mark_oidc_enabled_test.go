// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestMarkOIDCEnabled_LatchesFlag(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).
		Return(db.Cluster{ID: clusterID, OIDCAuthEnabled: false}, nil)
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID && c.OIDCAuthEnabled
	})).Return(db.Cluster{ID: clusterID, OIDCAuthEnabled: true}, nil)

	a := &Activities{db: mockDB}
	_, err := a.MarkOIDCEnabled(context.Background(), MarkOIDCEnabledInput{ClusterID: clusterID})
	require.NoError(t, err)
	mockTx.AssertCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestMarkOIDCEnabled_IdempotentSkipsWrite(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).
		Return(db.Cluster{ID: clusterID, OIDCAuthEnabled: true}, nil)

	a := &Activities{db: mockDB}
	_, err := a.MarkOIDCEnabled(context.Background(), MarkOIDCEnabledInput{ClusterID: clusterID})
	require.NoError(t, err)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestMarkOIDCEnabled_ClusterNotFound(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).
		Return(db.Cluster{}, db.ErrNotFound)

	a := &Activities{db: mockDB}
	_, err := a.MarkOIDCEnabled(context.Background(), MarkOIDCEnabledInput{ClusterID: clusterID})
	require.Error(t, err)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestMarkOIDCEnabled_ClearsLockAndFailureAtomically(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:              clusterID,
		OIDCAuthEnabled: false,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeOIDCEnable},
			UpdateFailed:     &db.Condition{Status: db.ConditionStatusTrue, Message: "prior attempt"},
		},
	}, nil)
	// The marker flip, lock release, and UpdateFailed clear all land in one write.
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID && c.OIDCAuthEnabled &&
			c.Conditions.UpdateInProgress == nil && c.Conditions.UpdateFailed == nil
	})).Return(db.Cluster{}, nil)

	a := &Activities{db: mockDB}
	_, err := a.MarkOIDCEnabled(context.Background(), MarkOIDCEnabledInput{ClusterID: clusterID})
	require.NoError(t, err)
	mockTx.AssertCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClearOIDCUpdateInProgress_ClearsLockAndUpdateFailedOnSuccess(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeOIDCEnable},
			UpdateFailed:     &db.Condition{Status: db.ConditionStatusTrue, Message: "prior attempt"},
		},
	}, nil)
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID &&
			c.Conditions.UpdateInProgress == nil &&
			c.Conditions.UpdateFailed == nil
	})).Return(db.Cluster{}, nil)

	a := &Activities{db: mockDB}
	_, err := a.ClearOIDCUpdateInProgress(context.Background(), ClearOIDCUpdateInProgressInput{ClusterID: clusterID})
	require.NoError(t, err)
	mockTx.AssertCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClearOIDCUpdateInProgress_RecordsFailure(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeOIDCEnable},
		},
	}, nil)
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.ID == clusterID &&
			c.Conditions.UpdateInProgress == nil &&
			c.Conditions.UpdateFailed != nil &&
			c.Conditions.UpdateFailed.Status == db.ConditionStatusTrue &&
			c.Conditions.UpdateFailed.Message == "OIDC enablement failed"
	})).Return(db.Cluster{}, nil)

	a := &Activities{db: mockDB}
	_, err := a.ClearOIDCUpdateInProgress(context.Background(), ClearOIDCUpdateInProgressInput{
		ClusterID:      clusterID,
		FailureMessage: "OIDC enablement failed",
	})
	require.NoError(t, err)
	mockTx.AssertCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClearOIDCUpdateInProgress_DoesNotClearOtherOperations(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeK8sUpgrade},
		},
	}, nil)

	a := &Activities{db: mockDB}
	_, err := a.ClearOIDCUpdateInProgress(context.Background(), ClearOIDCUpdateInProgressInput{
		ClusterID:      clusterID,
		FailureMessage: "OIDC enablement failed",
	})
	require.NoError(t, err)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

func TestClearOIDCUpdateInProgress_NoLockIsNoop(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).
		Return(db.Cluster{ID: clusterID}, nil)

	a := &Activities{db: mockDB}
	_, err := a.ClearOIDCUpdateInProgress(context.Background(), ClearOIDCUpdateInProgressInput{ClusterID: clusterID})
	require.NoError(t, err)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}
