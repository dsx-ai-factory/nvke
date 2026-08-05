// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"

	rolloutetcd "github.com/NVIDIA/nke/controller/internal/rollout/operations/etcd"
	rollouttemporal "github.com/NVIDIA/nke/controller/internal/rollout/temporal"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

const (
	etcdEncryptionKeyRotationFailureMessage = "Etcd encryption key rotation failed"
)

type RotateEtcdEncryptionKeyInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
}

// RotateEtcdEncryptionKey is an operator-initiated Temporal workflow. It is
// registered on the CWE worker but intentionally has no public NKE API route.
func RotateEtcdEncryptionKey(ctx workflow.Context, input RotateEtcdEncryptionKeyInput) error {
	if input.ClusterID == uuid.Nil {
		return errors.New("clusterID is required")
	}

	var a *activities.Activities
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.BeginEtcdEncryptionKeyRotation,
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to begin etcd encryption key rotation: %w", err)
	}

	logger := workflow.GetLogger(ctx)

	if err := rotateEtcdEncryptionKey(ctx, input); err != nil {
		cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
		if clearErr := clearEtcdEncryptionUpdateInProgress(
			cleanupCtx,
			input.ClusterID,
			etcdEncryptionKeyRotationFailureMessage,
		); clearErr != nil {
			logger.Error("failed to clear etcd encryption key rotation progress", "error", clearErr)
		}
		return fmt.Errorf("failed to rotate etcd encryption key: %w", err)
	}

	return nil
}

func rotateEtcdEncryptionKey(ctx workflow.Context, input RotateEtcdEncryptionKeyInput) error {
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
	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewAddSecondaryKeyOperation(),
	); err != nil {
		return err
	}
	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewPromotePrimaryKeyOperation(),
	); err != nil {
		return err
	}

	if err := rewriteEtcdEncryptedObjects(ctx, plan.ExecutorNodeID); err != nil {
		return err
	}

	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewRemoveOldKeysOperation(),
	); err != nil {
		return err
	}

	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.FinalizePendingEtcdEncryptionKey,
		activities.EtcdEncryptionClusterInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to finalize etcd encryption rotation key: %w", err)
	}

	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.CompleteEtcdEncryptionKeyRotation,
		activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to mark etcd encryption key rotation complete: %w", err)
	}

	return nil
}
