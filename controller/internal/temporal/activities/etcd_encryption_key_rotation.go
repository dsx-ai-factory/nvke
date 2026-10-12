// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type PrepareEtcdEncryptionKeyRotationInput struct {
	ClusterID uuid.UUID
}

type BeginEtcdEncryptionKeyRotationInput struct {
	ClusterID uuid.UUID
}

type BeginEtcdEncryptionKeyRotationResult struct{}

func (a *Activities) BeginEtcdEncryptionKeyRotation(
	ctx context.Context,
	input BeginEtcdEncryptionKeyRotationInput,
) (BeginEtcdEncryptionKeyRotationResult, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}
		if !cluster.EtcdEncrypted {
			return etcdEncryptionKeyRotationPreconditionError(
				fmt.Sprintf("etcd encryption is not enabled on cluster %s", input.ClusterID),
			)
		}
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("cannot rotate the etcd encryption key while cluster deletion is in progress")
		}
		if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("cannot rotate the etcd encryption key until the cluster is provisioned")
		}
		if update := cluster.Conditions.UpdateInProgress; update != nil {
			return etcdEncryptionKeyRotationPreconditionError(
				fmt.Sprintf("cannot rotate the etcd encryption key while %s is in progress", update.Type),
			)
		}
		if cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("cannot rotate the etcd encryption key while a control-plane upgrade is in progress")
		}
		if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("cannot rotate the etcd encryption key while control-plane node recycling is in progress")
		}

		db.ClearUpdateFailed(&cluster)
		db.SetUpdateInProgress(
			&cluster,
			db.OperationTypeEtcdEncryption,
			"rotating etcd encryption key",
			time.Now().UTC(),
		)
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to mark etcd encryption key rotation in progress: %w", err)
		}
		return nil
	})
	if err != nil {
		return BeginEtcdEncryptionKeyRotationResult{}, err
	}
	return BeginEtcdEncryptionKeyRotationResult{}, nil
}

func etcdEncryptionKeyRotationPreconditionError(message string) error {
	return temporal.NewNonRetryableApplicationError(
		message,
		"EtcdEncryptionKeyRotationPrecondition",
		nil,
	)
}

func (a *Activities) PrepareEtcdEncryptionKeyRotation(
	ctx context.Context,
	input PrepareEtcdEncryptionKeyRotationInput,
) (PrepareEtcdEncryptionResult, error) {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return PrepareEtcdEncryptionResult{}, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
		}
		return PrepareEtcdEncryptionResult{}, fmt.Errorf("failed to get cluster %s: %w", input.ClusterID, err)
	}
	if !cluster.EtcdEncrypted {
		return PrepareEtcdEncryptionResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("etcd encryption is not enabled on cluster %s", input.ClusterID),
			"EtcdEncryptionNotEnabled",
			nil,
		)
	}
	return a.prepareEtcdEncryptionControlPlaneNodes(ctx, input.ClusterID, "rotate the etcd encryption key")
}

func (a *Activities) EnsurePendingEtcdEncryptionRotationKey(ctx context.Context, input EtcdEncryptionClusterInput) error {
	_, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, input.ClusterID)
	return err
}

type CompleteEtcdEncryptionKeyRotationInput struct {
	ClusterID uuid.UUID
}

type CompleteEtcdEncryptionKeyRotationResult struct{}

func (a *Activities) CompleteEtcdEncryptionKeyRotation(
	ctx context.Context,
	input CompleteEtcdEncryptionKeyRotationInput,
) (CompleteEtcdEncryptionKeyRotationResult, error) {
	err := a.completeEtcdEncryptionOperation(ctx, input.ClusterID, false)
	if err != nil {
		return CompleteEtcdEncryptionKeyRotationResult{}, err
	}
	return CompleteEtcdEncryptionKeyRotationResult{}, nil
}
