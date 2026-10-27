// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// RecordEtcdKeyRotationPhaseInput carries a non-secret checkpoint and its owner.
type RecordEtcdKeyRotationPhaseInput struct {
	ClusterID  uuid.UUID
	WorkflowID string
	Phase      db.EtcdKeyRotationPhase
}

// RecordEtcdKeyRotationPhase persists a phase entry, preserving timestamps on
// activity redelivery. Resetting the original workflow can resume a failed phase.
func (a *Activities) RecordEtcdKeyRotationPhase(ctx context.Context, input RecordEtcdKeyRotationPhaseInput) error {
	if !input.Phase.Valid() {
		return etcdEncryptionKeyRotationPreconditionError("invalid etcd key rotation phase")
	}
	err := a.mutateEtcdKeyRotation(ctx, input.ClusterID, input.WorkflowID, func(cluster *db.Cluster, status *db.EtcdKeyRotationStatus) error {
		if status.State == db.EtcdKeyRotationStateSucceeded {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation already completed")
		}
		if !ownsEtcdKeyRotationLock(cluster) {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation does not own the cluster mutation lock")
		}
		// Cluster deletion takes precedence: stop at the next phase boundary.
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation stopped because cluster deletion is in progress")
		}
		if status.State == db.EtcdKeyRotationStateRunning && status.Phase == input.Phase {
			return nil
		}
		status.State = db.EtcdKeyRotationStateRunning
		status.Phase = input.Phase
		status.LastProgressAt = time.Now().UTC()
		status.CompletedAt = nil
		status.FailureReason = ""
		db.ClearUpdateFailed(cluster)
		return nil
	})
	outcome, reason := auditOperationOutcome(err)
	if err == nil {
		reason = credentialaudit.ReasonAccepted
	}
	event := credentialOperationAudit(input.ClusterID, nil, credentialaudit.ActionCheck,
		credentialaudit.TypeEtcdKey, outcome, reason)
	event.Phase = string(input.Phase)
	a.recordCredentialAudit(ctx, event, input.WorkflowID)
	return err
}

// FailEtcdKeyRotationInput intentionally excludes free-form errors and key data.
type FailEtcdKeyRotationInput struct {
	ClusterID  uuid.UUID
	WorkflowID string
	Reason     db.EtcdKeyRotationFailureReason
}

// FailEtcdKeyRotation releases the owner lock only in prepare, before any node
// receives the pending key. Later phases retain it for original-workflow recovery.
// Delayed cleanup cannot change a newer owner or an already-completed rotation.
func (a *Activities) FailEtcdKeyRotation(ctx context.Context, input FailEtcdKeyRotationInput) error {
	if input.Reason != db.EtcdKeyRotationFailureOperation && input.Reason != db.EtcdKeyRotationFailureCancelled {
		return etcdEncryptionKeyRotationPreconditionError("invalid etcd key rotation failure reason")
	}
	phase := ""
	outcome, reason := credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed
	err := a.mutateEtcdKeyRotation(ctx, input.ClusterID, input.WorkflowID, func(cluster *db.Cluster, status *db.EtcdKeyRotationStatus) error {
		phase = string(status.Phase)
		if status.State == db.EtcdKeyRotationStateSucceeded || status.State == db.EtcdKeyRotationStateFailed {
			if status.State == db.EtcdKeyRotationStateSucceeded {
				outcome, reason = credentialaudit.OutcomeNoop, credentialaudit.ReasonUnchanged
			}
			return nil
		}
		if !ownsEtcdKeyRotationLock(cluster) {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation does not own the cluster mutation lock")
		}
		now := time.Now().UTC()
		status.State = db.EtcdKeyRotationStateFailed
		status.FailureReason = input.Reason
		status.CompletedAt = &now
		status.LastProgressAt = now
		if status.Phase == db.EtcdKeyRotationPhasePrepare {
			// Begin refuses a reset of this owner once its lock is released.
			db.ClearUpdateInProgress(cluster)
			db.SetUpdateFailed(cluster, "Etcd encryption key rotation failed in prepare; fix the prerequisite and start a new rotation with a new workflow ID")
			return nil
		}
		db.SetUpdateFailed(cluster, "Etcd encryption key rotation failed; recover the owning workflow")
		return nil
	})
	event := credentialOperationAudit(input.ClusterID, nil, credentialaudit.ActionRotate,
		credentialaudit.TypeEtcdKey, outcome, reason)
	event.Phase = phase
	a.recordCredentialAudit(ctx, event, input.WorkflowID)
	return err
}

func (a *Activities) completeEtcdKeyRotation(ctx context.Context, clusterID uuid.UUID, workflowID string) error {
	return a.mutateEtcdKeyRotation(ctx, clusterID, workflowID, func(cluster *db.Cluster, status *db.EtcdKeyRotationStatus) error {
		if status.State == db.EtcdKeyRotationStateSucceeded {
			return nil
		}
		if !ownsEtcdKeyRotationLock(cluster) || status.Phase != db.EtcdKeyRotationPhaseFinalize {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation cannot complete before finalization under its lock")
		}
		now := time.Now().UTC()
		status.State = db.EtcdKeyRotationStateSucceeded
		status.CompletedAt = &now
		status.LastProgressAt = now
		status.FailureReason = ""
		db.ClearUpdateInProgress(cluster)
		db.ClearUpdateFailed(cluster)
		return nil
	})
}

func ownsEtcdKeyRotationLock(cluster *db.Cluster) bool {
	return cluster.Conditions.UpdateInProgress != nil && cluster.Conditions.UpdateInProgress.Type == db.OperationTypeEtcdKeyRotation
}

func (a *Activities) mutateEtcdKeyRotation(ctx context.Context, clusterID uuid.UUID, workflowID string, mutate func(*db.Cluster, *db.EtcdKeyRotationStatus) error) error {
	if workflowID == "" {
		return etcdEncryptionKeyRotationPreconditionError("etcd key rotation workflow ID is required")
	}
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, clusterID)
		if err != nil {
			// A deleted cluster cannot be updated; retrying would never close the workflow.
			return nonRetryableIfNotFound("cluster not found for etcd key rotation", fmt.Errorf("get cluster for etcd key rotation: %w", err))
		}
		status := cluster.Conditions.EtcdKeyRotation
		if status == nil || status.WorkflowID != workflowID {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation workflow no longer owns this cluster")
		}
		if err := mutate(&cluster, status); err != nil {
			return err
		}
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("persist etcd key rotation: %w", err)
		}
		return nil
	})
}

// FinalizeEtcdRotationKey promotes persisted key state inside an activity. It
// redacts upstream errors, which can include rejected Secret bodies, before the
// Temporal SDK serializes them. Key material never appears in activity results.
func (a *Activities) FinalizeEtcdRotationKey(ctx context.Context, input EtcdEncryptionClusterInput) error {
	if err := a.finalizePendingKubeAPIEncryptionKey(ctx, input.ClusterID, pendingKeyOwnedRotation); err != nil {
		return errors.New("finalizing etcd rotation key failed; inspect Kubernetes access and key state")
	}
	return nil
}
