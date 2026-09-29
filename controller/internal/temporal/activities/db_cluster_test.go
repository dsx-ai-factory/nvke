// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestUpdateClusterConditionsLocksAndPreservesOtherConditions(t *testing.T) {
	for _, status := range []db.ConditionStatus{db.ConditionStatusTrue, db.ConditionStatusFalse, db.ConditionStatusUnknown} {
		t.Run(string(status), func(t *testing.T) {
			clusterID := uuid.New()
			updateFailed := db.Condition{Status: status, Message: "previous update failure", LastTransitionTime: time.Now()}
			upgrading := db.NewCondition(true, "Upgrade in progress")
			database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			database.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
				ID:         clusterID,
				Conditions: db.ClusterConditions{UpdateFailed: &updateFailed, ControlPlaneUpgrading: upgrading},
			}, nil).Once()
			database.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
				return c.ID == clusterID && c.Conditions.UpdateFailed.Equal(updateFailed) &&
					c.Conditions.ControlPlaneUpgrading.Equal(upgrading) &&
					c.Conditions.Provisioned.Status == db.ConditionStatusTrue && c.Conditions.Provisioned.Message == "Provisioned"
			})).Return(db.Cluster{}, nil).Once()
			a := &Activities{db: database}
			err := a.UpdateClusterConditions(context.Background(), ClusterConditionsUpdate{
				ClusterID:  clusterID,
				Conditions: ClusterConditions{Provisioned: &ConditionUpdate{Status: db.ConditionStatusTrue, Message: "Provisioned"}},
			})
			require.NoError(t, err)
			database.AssertExpectations(t)
			database.MockTx.AssertExpectations(t)
		})
	}
}

func TestUpdateClusterConditionsErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readErr  error
		writeErr error
	}{
		{name: "missing", readErr: db.ErrNotFound},
		{name: "read failed", readErr: errors.New("read failed")},
		{name: "write failed", writeErr: errors.New("write failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clusterID := uuid.New()
			database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			database.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, tc.readErr).Once()
			if tc.readErr == nil {
				database.MockTx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, tc.writeErr).Once()
			}
			a := &Activities{db: database}
			err := a.UpdateClusterConditions(context.Background(), ClusterConditionsUpdate{ClusterID: clusterID})
			require.Error(t, err)
			switch {
			case errors.Is(tc.readErr, db.ErrNotFound):
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				require.True(t, appErr.NonRetryable())
			case tc.readErr != nil:
				require.ErrorIs(t, err, tc.readErr)
			default:
				require.ErrorIs(t, err, tc.writeErr)
			}
			database.AssertExpectations(t)
			database.MockTx.AssertExpectations(t)
		})
	}
}
