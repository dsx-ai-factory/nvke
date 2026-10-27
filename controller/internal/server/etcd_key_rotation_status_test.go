// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
)

// TestAPIRotationStatusShowsFailureIndependentlyOfRetainedLock protects the
// operator response when Temporal has closed but recovery still owns mutation.
func TestAPIRotationStatusShowsFailureIndependentlyOfRetainedLock(t *testing.T) {
	now := time.Now().UTC()
	cluster := db.Cluster{Conditions: db.ClusterConditions{
		EtcdKeyRotation: &db.EtcdKeyRotationStatus{
			WorkflowID: "rotation-owner", State: db.EtcdKeyRotationStateFailed,
			Phase: db.EtcdKeyRotationPhaseRewrite, StartedAt: now,
			LastProgressAt: now, CompletedAt: &now, FailureReason: db.EtcdKeyRotationFailureOperation,
		},
		UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation, StartedAt: now},
	}}
	response := server.APIClusterFromDB(&cluster)
	require.NotNil(t, response.Conditions.EtcdKeyRotation)
	require.Equal(t, api.EtcdKeyRotationStatusStateFailed, response.Conditions.EtcdKeyRotation.State)
	require.Equal(t, api.EtcdKeyRotationStatusPhaseRewrite, response.Conditions.EtcdKeyRotation.Phase)
	require.Equal(t, "rotation-owner", response.Conditions.EtcdKeyRotation.WorkflowID)
	require.Equal(t, now, response.Conditions.EtcdKeyRotation.LastProgressAt)
	require.NotNil(t, response.Conditions.EtcdKeyRotation.FailureReason)
	require.Equal(t, api.EtcdKeyRotationStatusFailureReasonOperationFailed, *response.Conditions.EtcdKeyRotation.FailureReason)
	require.NotNil(t, response.Conditions.UpdateInProgress)
	require.Equal(t, api.UpdateInProgressTypeEtcdKeyRotation, response.Conditions.UpdateInProgress.Type)

	cluster.Conditions.EtcdKeyRotation.State = db.EtcdKeyRotationStateSucceeded
	cluster.Conditions.EtcdKeyRotation.FailureReason = ""
	cluster.Conditions.UpdateInProgress = nil
	response = server.APIClusterFromDB(&cluster)
	require.Nil(t, response.Conditions.UpdateInProgress)
	require.Nil(t, response.Conditions.EtcdKeyRotation.FailureReason)
	legacy := server.APIClusterFromDB(&db.Cluster{})
	require.Nil(t, legacy.Conditions.EtcdKeyRotation)
}
