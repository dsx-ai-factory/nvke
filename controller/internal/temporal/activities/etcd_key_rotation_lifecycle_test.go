// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

type rotationTx struct {
	db.Tx
	cluster *db.Cluster
	writes  int
	deleted bool
}

func (tx *rotationTx) GetClusterForUpdate(context.Context, uuid.UUID) (db.Cluster, error) {
	if tx.deleted {
		return db.Cluster{}, db.ErrNotFound
	}
	cluster := *tx.cluster
	if status := cluster.Conditions.EtcdKeyRotation; status != nil {
		copyStatus := *status
		cluster.Conditions.EtcdKeyRotation = &copyStatus
	}
	return cluster, nil
}

func (tx *rotationTx) ListNodePools(context.Context, db.NodePoolFilters) ([]db.NodePool, error) {
	return nil, nil
}

func (tx *rotationTx) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	return []db.Node{stableRotationNode(tx.cluster.ID)}, nil
}

func (tx *rotationTx) UpdateCluster(_ context.Context, updated *db.Cluster) (db.Cluster, error) {
	*tx.cluster = *updated
	tx.writes++
	return *updated, nil
}

func rotationActivities(t *testing.T, cluster *db.Cluster) (*activities.Activities, *rotationTx) {
	t.Helper()
	tx := &rotationTx{cluster: cluster}
	store := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
	store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	return activities.NewActivities(config.Config{}, store, nil, nil, "", nil, ""), tx
}

func runningRotation() db.Cluster {
	now := time.Now().UTC().Add(-time.Hour)
	return db.Cluster{ID: uuid.New(), EtcdEncrypted: true, Conditions: db.ClusterConditions{
		Provisioned:      db.NewCondition(true, "ready"),
		UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation, StartedAt: now},
		EtcdKeyRotation: &db.EtcdKeyRotationStatus{
			WorkflowID: "owner", State: db.EtcdKeyRotationStateRunning,
			Phase: db.EtcdKeyRotationPhasePrepare, StartedAt: now, LastProgressAt: now,
		},
	}, KubernetesVersion: "1.34.0"}
}

func TestBeginEtcdEncryptionKeyRotationRequiresKubernetes134(t *testing.T) {
	for _, tc := range []struct {
		version string
		allowed bool
	}{
		{version: "1.33.9"},
		{version: "v1.33.0"},
		{version: "1.34.0", allowed: true},
		{version: "v1.34.2", allowed: true},
		{version: "1.35.0", allowed: true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			cluster := runningRotation()
			cluster.KubernetesVersion = tc.version
			cluster.Conditions.EtcdKeyRotation = nil
			cluster.Conditions.UpdateInProgress = nil
			a, tx := rotationActivities(t, &cluster)
			_, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{
				ClusterID: cluster.ID, WorkflowID: "owner",
			})
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, db.OperationTypeEtcdKeyRotation, cluster.Conditions.UpdateInProgress.Type)
				return
			}
			require.ErrorContains(t, err, "Kubernetes 1.34 or newer")
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.Equal(t, "EtcdEncryptionKeyRotationPrecondition", applicationErr.Type())
			require.True(t, applicationErr.NonRetryable())
			require.Zero(t, tx.writes)
			require.Nil(t, cluster.Conditions.UpdateInProgress)
			require.Nil(t, cluster.Conditions.EtcdKeyRotation)
		})
	}
}

func TestFailEtcdKeyRotationReleasesOnlyPrepareLock(t *testing.T) {
	for _, phase := range []db.EtcdKeyRotationPhase{
		db.EtcdKeyRotationPhasePrepare,
		db.EtcdKeyRotationPhaseAddSecondary,
		db.EtcdKeyRotationPhasePromotePrimary,
		db.EtcdKeyRotationPhaseRewrite,
		db.EtcdKeyRotationPhaseRemoveOldKeys,
		db.EtcdKeyRotationPhaseFinalize,
	} {
		t.Run(string(phase), func(t *testing.T) {
			cluster := runningRotation()
			cluster.Conditions.EtcdKeyRotation.Phase = phase
			a, _ := rotationActivities(t, &cluster)
			input := activities.FailEtcdKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation}
			require.NoError(t, a.FailEtcdKeyRotation(t.Context(), input))
			status := cluster.Conditions.EtcdKeyRotation
			require.Equal(t, db.EtcdKeyRotationStateFailed, status.State)
			require.Equal(t, phase, status.Phase)
			require.Equal(t, db.EtcdKeyRotationFailureOperation, status.FailureReason)
			require.NotNil(t, status.CompletedAt)
			require.Equal(t, db.ConditionStatusTrue, cluster.Conditions.UpdateFailed.Status)
			if phase == db.EtcdKeyRotationPhasePrepare {
				require.Nil(t, cluster.Conditions.UpdateInProgress)
			} else {
				require.Equal(t, db.OperationTypeEtcdKeyRotation, cluster.Conditions.UpdateInProgress.Type)
			}
			completed := status.CompletedAt
			require.NoError(t, a.FailEtcdKeyRotation(t.Context(), input))
			require.Equal(t, completed, cluster.Conditions.EtcdKeyRotation.CompletedAt)
		})
	}
}

func TestPrepareFailureNewRotationReusesStagedPendingKey(t *testing.T) {
	cluster := runningRotation()
	store := &fenceStore{cluster: cluster, nodes: []db.Node{stableRotationNode(cluster.ID)}}
	secretKey := client.ObjectKey{Namespace: "cloud-workflow-engine", Name: constants.EtcdEncryptionKeySecretName(cluster.ID)}
	k8sClient := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: secretKey.Namespace, Name: secretKey.Name},
		Data: map[string][]byte{
			constants.EtcdEncryptionKeyFieldActive: []byte("active-key"),
		},
	}).Build()
	activeDigest := sha256.Sum256([]byte("active-key"))
	nodeAgent := &agenttest.FakeClient{GetEtcdEncryptionKeyIdentityHook: func(_ db.Node, digest string) (string, error) {
		if digest == hex.EncodeToString(activeDigest[:]) {
			return constants.LegacyAPIEncryptionKeyName, nil
		}
		return "", nil // prepare has not distributed the pending key to any node
	}}
	a := activities.NewActivities(config.Config{PodNamespace: secretKey.Namespace}, store, k8sClient, nodeAgent, "", nil, "")
	input := activities.EtcdEncryptionClusterInput{ClusterID: cluster.ID}
	require.NoError(t, a.EnsurePendingEtcdEncryptionRotationKey(t.Context(), input))
	var staged corev1.Secret
	require.NoError(t, k8sClient.Get(t.Context(), secretKey, &staged))
	require.NotEmpty(t, staged.Data[constants.EtcdEncryptionKeyFieldPending])
	require.NotEmpty(t, staged.Data[constants.EtcdEncryptionKeyFieldPendingName])
	require.NotEqual(t, staged.Data[constants.EtcdEncryptionKeyFieldActive], staged.Data[constants.EtcdEncryptionKeyFieldPending])

	require.NoError(t, a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{
		ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation,
	}))
	require.Nil(t, store.cluster.Conditions.UpdateInProgress)
	require.ErrorContains(t, a.RecordEtcdKeyRotationPhase(t.Context(), activities.RecordEtcdKeyRotationPhaseInput{
		ClusterID: cluster.ID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhasePrepare,
	}), "does not own the cluster mutation lock")
	_, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{
		ClusterID: cluster.ID, WorkflowID: "new-owner",
	})
	require.NoError(t, err)
	require.Equal(t, "new-owner", store.cluster.Conditions.EtcdKeyRotation.WorkflowID)
	require.Equal(t, db.EtcdKeyRotationStateRunning, store.cluster.Conditions.EtcdKeyRotation.State)
	require.Equal(t, db.EtcdKeyRotationPhasePrepare, store.cluster.Conditions.EtcdKeyRotation.Phase)
	require.Nil(t, store.cluster.Conditions.UpdateFailed)
	require.NoError(t, a.EnsurePendingEtcdEncryptionRotationKey(t.Context(), input))
	var reused corev1.Secret
	require.NoError(t, k8sClient.Get(t.Context(), secretKey, &reused))
	require.Equal(t, staged.Data, reused.Data)
}

// TestRotationBeginRetryKeepsOwnership protects a committed begin whose activity
// acknowledgement was lost: retry must neither fail nor start a second cycle.
func TestRotationBeginRetryKeepsOwnership(t *testing.T) {
	cluster := runningRotation()
	cluster.Conditions.UpdateInProgress = nil
	cluster.Conditions.EtcdKeyRotation = nil
	a, _ := rotationActivities(t, &cluster)
	input := activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner"}
	_, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), input)
	require.NoError(t, err)
	started := cluster.Conditions.EtcdKeyRotation.StartedAt
	_, err = a.BeginEtcdEncryptionKeyRotation(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, started, cluster.Conditions.EtcdKeyRotation.StartedAt)
	require.Equal(t, db.OperationTypeEtcdKeyRotation, cluster.Conditions.UpdateInProgress.Type)
	_, err = a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "another"})
	require.ErrorContains(t, err, "in progress")
}

// TestRotationPhaseRedeliveryAndRecovery distinguishes phase progress from polling
// and ensures reset recovery retains the same key lifecycle and owner.
func TestRotationPhaseRedeliveryAndRecovery(t *testing.T) {
	cluster := runningRotation()
	a, _ := rotationActivities(t, &cluster)
	input := activities.RecordEtcdKeyRotationPhaseInput{ClusterID: cluster.ID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseRewrite}
	require.NoError(t, a.RecordEtcdKeyRotationPhase(t.Context(), input))
	progress := cluster.Conditions.EtcdKeyRotation.LastProgressAt
	require.NoError(t, a.RecordEtcdKeyRotationPhase(t.Context(), input))
	require.Equal(t, progress, cluster.Conditions.EtcdKeyRotation.LastProgressAt)
	require.NoError(t, a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation}))
	require.Equal(t, db.EtcdKeyRotationStateFailed, cluster.Conditions.EtcdKeyRotation.State)
	require.NotNil(t, cluster.Conditions.UpdateInProgress)
	failedAt := cluster.Conditions.EtcdKeyRotation.CompletedAt
	require.NoError(t, a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation}))
	require.Equal(t, failedAt, cluster.Conditions.EtcdKeyRotation.CompletedAt)
	require.NoError(t, a.RecordEtcdKeyRotationPhase(t.Context(), input))
	require.Equal(t, db.EtcdKeyRotationStateRunning, cluster.Conditions.EtcdKeyRotation.State)
	require.Nil(t, cluster.Conditions.EtcdKeyRotation.CompletedAt)
	require.Nil(t, cluster.Conditions.UpdateFailed)
}

func TestRotationCompletionRequiresFinalizationAndIsIdempotent(t *testing.T) {
	cluster := runningRotation()
	a, _ := rotationActivities(t, &cluster)
	input := activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner"}
	_, err := a.CompleteEtcdEncryptionKeyRotation(t.Context(), input)
	require.ErrorContains(t, err, "before finalization")
	require.NoError(t, a.RecordEtcdKeyRotationPhase(t.Context(), activities.RecordEtcdKeyRotationPhaseInput{
		ClusterID: cluster.ID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseFinalize,
	}))
	_, err = a.CompleteEtcdEncryptionKeyRotation(t.Context(), input)
	require.NoError(t, err)
	require.True(t, cluster.EtcdEncrypted)
	require.Nil(t, cluster.Conditions.UpdateInProgress)
	require.Equal(t, db.EtcdKeyRotationStateSucceeded, cluster.Conditions.EtcdKeyRotation.State)
	completed := cluster.Conditions.EtcdKeyRotation.CompletedAt
	_, err = a.CompleteEtcdEncryptionKeyRotation(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, completed, cluster.Conditions.EtcdKeyRotation.CompletedAt)
	result, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner"})
	require.NoError(t, err)
	require.True(t, result.AlreadyCompleted)
}

func TestRotationDelayedUpdatesCannotTouchAnotherOwner(t *testing.T) {
	for _, action := range []string{"phase", "failure", "completion"} {
		t.Run(action, func(t *testing.T) {
			cluster := runningRotation()
			a, store := rotationActivities(t, &cluster)
			var err error
			switch action {
			case "phase":
				err = a.RecordEtcdKeyRotationPhase(t.Context(), activities.RecordEtcdKeyRotationPhaseInput{ClusterID: cluster.ID, WorkflowID: "old-owner", Phase: db.EtcdKeyRotationPhaseRewrite})
			case "failure":
				err = a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "old-owner", Reason: db.EtcdKeyRotationFailureOperation})
			case "completion":
				_, err = a.CompleteEtcdEncryptionKeyRotation(t.Context(), activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "old-owner"})
			}
			require.ErrorContains(t, err, "no longer owns")
			require.Zero(t, store.writes)
			require.Equal(t, "owner", cluster.Conditions.EtcdKeyRotation.WorkflowID)
		})
	}
}

// TestRotationCompletionAfterResetDoesNotRequireReplayedCheckpointWrites models
// a Temporal reset before completion: earlier successful checkpoints replay
// without redoing their database writes, so status may still be failed/finalize.
func TestRotationCompletionAfterResetDoesNotRequireReplayedCheckpointWrites(t *testing.T) {
	cluster := runningRotation()
	cluster.Conditions.EtcdKeyRotation.Phase = db.EtcdKeyRotationPhaseFinalize
	a, _ := rotationActivities(t, &cluster)
	require.NoError(t, a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{
		ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation,
	}))
	_, err := a.CompleteEtcdEncryptionKeyRotation(t.Context(), activities.CompleteEtcdEncryptionKeyRotationInput{
		ClusterID: cluster.ID, WorkflowID: "owner",
	})
	require.NoError(t, err)
	require.Equal(t, db.EtcdKeyRotationStateSucceeded, cluster.Conditions.EtcdKeyRotation.State)
	require.Nil(t, cluster.Conditions.UpdateInProgress)
	completed := cluster.Conditions.EtcdKeyRotation.CompletedAt
	require.NoError(t, a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{
		ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation,
	}))
	require.Equal(t, db.EtcdKeyRotationStateSucceeded, cluster.Conditions.EtcdKeyRotation.State)
	require.Equal(t, completed, cluster.Conditions.EtcdKeyRotation.CompletedAt)
}

// TestRotationLifecycleStopsRetryingForDeletedCluster: teardown hard-deletes the
// row, so the lifecycle activities must not retry forever under unlimited DB retries.
func TestRotationLifecycleStopsRetryingForDeletedCluster(t *testing.T) {
	for _, action := range []string{"phase", "failure", "completion"} {
		t.Run(action, func(t *testing.T) {
			cluster := runningRotation()
			a, store := rotationActivities(t, &cluster)
			store.deleted = true
			var err error
			switch action {
			case "phase":
				err = a.RecordEtcdKeyRotationPhase(t.Context(), activities.RecordEtcdKeyRotationPhaseInput{ClusterID: cluster.ID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseRewrite})
			case "failure":
				err = a.FailEtcdKeyRotation(t.Context(), activities.FailEtcdKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation})
			case "completion":
				_, err = a.CompleteEtcdEncryptionKeyRotation(t.Context(), activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner"})
			}
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.True(t, applicationErr.NonRetryable())
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Zero(t, store.writes)
		})
	}
}

// TestBeginEtcdKeyRotationRefusesNodeAgentInstallation keeps an accepted agent
// installation from restarting agents under the rotation lock.
func TestBeginEtcdKeyRotationRefusesNodeAgentInstallation(t *testing.T) {
	cluster := runningRotation()
	cluster.Conditions.EtcdKeyRotation = nil
	cluster.Conditions.UpdateInProgress = nil
	cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{NodeID: uuid.New()}
	a, store := rotationActivities(t, &cluster)
	_, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: cluster.ID, WorkflowID: "owner"})
	require.ErrorContains(t, err, "node-agent installation is in progress")
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationErr)
	require.Equal(t, "EtcdEncryptionKeyRotationPrecondition", applicationErr.Type())
	require.Zero(t, store.writes)
	require.Nil(t, cluster.Conditions.UpdateInProgress)
}
