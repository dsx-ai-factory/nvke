// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	rolloutetcd "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/etcd"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	etcdEncryptionKeyRotationFailureMessage = "Etcd encryption key rotation failed"
	etcdKeyRotationLifecycleChangeID        = "etcd-key-rotation-lifecycle"
	etcdKeyRotationBeginAcceptanceChangeID  = "etcd-key-rotation-begin-acceptance"
	etcdKeyRotationMembershipChangeID       = "etcd-key-rotation-membership-convergence"
)

type RotateEtcdEncryptionKeyInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
}

// RotateEtcdEncryptionKey is an operator-initiated Temporal workflow. It is
// registered on the controller worker but intentionally has no public NVKE API route.
func RotateEtcdEncryptionKey(ctx workflow.Context, input RotateEtcdEncryptionKeyInput) error {
	if input.ClusterID == uuid.Nil {
		return errors.New("clusterID is required")
	}

	owner := ""
	if workflow.GetVersion(ctx, etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		owner = workflow.GetInfo(ctx).WorkflowExecution.ID
	}
	var a *activities.Activities
	var begin activities.BeginEtcdEncryptionKeyRotationResult
	// Begin may already have committed ownership, so new executions resolve acceptance even after
	// cancellation; older histories keep the cancellable command sequence.
	acceptanceCtx := ctx
	resolvesAcceptance := owner != "" && workflow.GetVersion(ctx, etcdKeyRotationBeginAcceptanceChangeID, workflow.DefaultVersion, 1) != workflow.DefaultVersion
	if resolvesAcceptance {
		acceptanceCtx, _ = workflow.NewDisconnectedContext(ctx)
	}
	if err := workflow.ExecuteActivity(
		withDBOptions(acceptanceCtx),
		a.BeginEtcdEncryptionKeyRotation,
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: input.ClusterID, WorkflowID: owner},
	).Get(acceptanceCtx, &begin); err != nil {
		return fmt.Errorf("failed to begin etcd encryption key rotation: %w", err)
	}

	if begin.AlreadyCompleted {
		return nil
	}
	logger := workflow.GetLogger(ctx)

	// A cancellation that arrived while acceptance was resolving must not dispatch node work.
	var err error
	if resolvesAcceptance {
		err = ctx.Err()
	}
	if err == nil {
		err = rotateEtcdEncryptionKey(ctx, input, owner)
	}
	if err != nil {
		cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
		if owner != "" {
			reason := db.EtcdKeyRotationFailureOperation
			if temporal.IsCanceledError(err) {
				reason = db.EtcdKeyRotationFailureCancelled
			}
			if recordErr := workflow.ExecuteActivity(withDBOptions(cleanupCtx), a.FailEtcdKeyRotation,
				activities.FailEtcdKeyRotationInput{ClusterID: input.ClusterID, WorkflowID: owner, Reason: reason},
			).Get(cleanupCtx, nil); recordErr != nil {
				logger.Error("failed to record etcd key rotation failure; mutation lock retained")
			}
		} else if clearErr := clearEtcdEncryptionUpdateInProgress(cleanupCtx, input.ClusterID, etcdEncryptionKeyRotationFailureMessage); clearErr != nil {
			logger.Error("failed to clear etcd encryption key rotation progress", "error", clearErr)
		}
		return fmt.Errorf("failed to rotate etcd encryption key: %w", err)
	}

	return nil
}

func rotateEtcdEncryptionKey(ctx workflow.Context, input RotateEtcdEncryptionKeyInput, owner string) error {
	var a *activities.Activities
	var plan activities.PrepareEtcdEncryptionResult
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.PrepareEtcdEncryptionKeyRotation,
		activities.PrepareEtcdEncryptionKeyRotationInput{ClusterID: input.ClusterID},
	).Get(ctx, &plan); err != nil {
		return fmt.Errorf("failed to prepare etcd encryption key rotation: %w", err)
	}

	if err := validateEtcdEncryptionHealth(ctx, input.ClusterID, plan.NodeIDs); err != nil {
		return err
	}

	cluster, err := rollouttemporal.LoadClusterForWorkflow(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to load cluster for etcd encryption key rotation: %w", err)
	}

	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.EnsurePendingEtcdEncryptionRotationKey,
		activities.EtcdEncryptionClusterInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to ensure pending etcd encryption rotation key: %w", err)
	}

	// UpdateEncryptionConfig hot-reloads the local apiserver configuration and
	// does not restart or drain the node, so these phases intentionally omit the
	// kube-vip drain/restore node guard.
	if err := recordEtcdKeyRotationPhase(ctx, input.ClusterID, owner, db.EtcdKeyRotationPhaseAddSecondary); err != nil {
		return err
	}
	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewAddSecondaryKeyOperation(),
	); err != nil {
		return err
	}
	if err := recordEtcdKeyRotationPhase(ctx, input.ClusterID, owner, db.EtcdKeyRotationPhasePromotePrimary); err != nil {
		return err
	}
	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewPromotePrimaryKeyOperation(),
	); err != nil {
		return err
	}

	if err := recordEtcdKeyRotationPhase(ctx, input.ClusterID, owner, db.EtcdKeyRotationPhaseRewrite); err != nil {
		return err
	}
	if err := rewriteEtcdEncryptedObjects(ctx, plan.ExecutorNodeID); err != nil {
		return err
	}
	// Keep old command histories unchanged. New executions must prove the full
	// live membership, re-confirm pending-primary config/reload on every node,
	// then recheck readiness immediately before discarding any decryptor.
	if owner != "" && workflow.GetVersion(ctx, etcdKeyRotationMembershipChangeID, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		if err := validateRotationMembership(ctx, input.ClusterID, owner, plan.NodeIDs); err != nil {
			return err
		}
		if err := executeEtcdEncryptionRolloutPhase(ctx, cluster, rolloutetcd.NewPromotePrimaryKeyOperation()); err != nil {
			return err
		}
		if err := validateRotationMembership(ctx, input.ClusterID, owner, plan.NodeIDs); err != nil {
			return err
		}
	}

	if err := recordEtcdKeyRotationPhase(ctx, input.ClusterID, owner, db.EtcdKeyRotationPhaseRemoveOldKeys); err != nil {
		return err
	}
	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewRemoveOldKeysOperation(),
	); err != nil {
		return err
	}

	if err := recordEtcdKeyRotationPhase(ctx, input.ClusterID, owner, db.EtcdKeyRotationPhaseFinalize); err != nil {
		return err
	}
	finalize := a.FinalizePendingEtcdEncryptionKey
	if owner != "" {
		finalize = a.FinalizeEtcdRotationKey
	}
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		finalize,
		activities.EtcdEncryptionClusterInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to finalize etcd encryption rotation key: %w", err)
	}

	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.CompleteEtcdEncryptionKeyRotation,
		activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: input.ClusterID, WorkflowID: owner},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to mark etcd encryption key rotation complete: %w", err)
	}

	return nil
}

func validateRotationMembership(ctx workflow.Context, clusterID uuid.UUID, owner string, nodeIDs []uuid.UUID) error {
	var a *activities.Activities
	if err := workflow.ExecuteActivity(withEtcdEncryptionOptions(ctx), a.ValidateEtcdRotationMembership,
		activities.ValidateEtcdRotationMembershipInput{ClusterID: clusterID, WorkflowID: owner, ExpectedNodeIDs: nodeIDs},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("cannot remove old encryption keys before control-plane convergence: %w", err)
	}
	return nil
}

func recordEtcdKeyRotationPhase(ctx workflow.Context, clusterID uuid.UUID, owner string, phase db.EtcdKeyRotationPhase) error {
	if owner == "" { // replay old histories without adding activity commands
		return nil
	}
	var a *activities.Activities
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.RecordEtcdKeyRotationPhase,
		activities.RecordEtcdKeyRotationPhaseInput{ClusterID: clusterID, WorkflowID: owner, Phase: phase},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("record etcd key rotation phase: %w", err)
	}
	return nil
}
