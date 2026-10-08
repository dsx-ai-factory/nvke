// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestFailedRotationCannotRestartAtBeginWithSameOwner(t *testing.T) {
	cluster := runningRotation()
	cluster.Conditions.EtcdKeyRotation.State = db.EtcdKeyRotationStateFailed
	cluster.Conditions.EtcdKeyRotation.Phase = db.EtcdKeyRotationPhaseFinalize
	a, tx := rotationActivities(t, &cluster)
	_, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{
		ClusterID: cluster.ID, WorkflowID: cluster.Conditions.EtcdKeyRotation.WorkflowID,
	})
	require.ErrorContains(t, err, "requires reset at its failed phase")
	require.Zero(t, tx.writes)
	require.Equal(t, db.EtcdKeyRotationStateFailed, cluster.Conditions.EtcdKeyRotation.State)
	require.Equal(t, db.EtcdKeyRotationPhaseFinalize, cluster.Conditions.EtcdKeyRotation.Phase)
	require.NotNil(t, cluster.Conditions.UpdateInProgress)
}

// TestLegacyRotationPayloadsRemainUnchanged preserves old activity command
// payloads when the versioned workflow omits lifecycle ownership.
func TestLegacyRotationPayloadsRemainUnchanged(t *testing.T) {
	clusterID := uuid.New()
	legacy, err := json.Marshal(struct {
		ClusterID uuid.UUID `json:"ClusterID"`
	}{ClusterID: clusterID})
	require.NoError(t, err)
	for _, input := range []any{
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
		activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	} {
		encoded, err := json.Marshal(input)
		require.NoError(t, err)
		require.JSONEq(t, string(legacy), string(encoded))
	}
	result, err := json.Marshal(activities.BeginEtcdEncryptionKeyRotationResult{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(result))
}
