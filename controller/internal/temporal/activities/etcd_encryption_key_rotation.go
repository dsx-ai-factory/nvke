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
	"k8s.io/apimachinery/pkg/util/version"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type PrepareEtcdEncryptionKeyRotationInput struct {
	ClusterID uuid.UUID
}

type BeginEtcdEncryptionKeyRotationInput struct {
	ClusterID  uuid.UUID
	WorkflowID string `json:"WorkflowID,omitempty"`
}

// BeginEtcdEncryptionKeyRotationResult keeps AlreadyCompleted only so recorded
// histories that carry it still decode and replay; Begin no longer sets it.
type BeginEtcdEncryptionKeyRotationResult struct {
	AlreadyCompleted bool `json:"AlreadyCompleted,omitempty"`
}

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
		if input.WorkflowID != "" {
			if status := cluster.Conditions.EtcdKeyRotation; status != nil && status.WorkflowID == input.WorkflowID {
				if status.State == db.EtcdKeyRotationStateSucceeded {
					// Begin succeeds at most once per run, so a succeeded owner means a
					// new execution reused the ID; it must not report success unrotated.
					return etcdEncryptionKeyRotationPreconditionError("etcd key rotation with this workflow ID already succeeded; start a new rotation with a new workflow ID")
				}
				if status.State == db.EtcdKeyRotationStateFailed && status.Phase == db.EtcdKeyRotationPhasePrepare {
					// A prepare failure released the lock, so no reset of this workflow can succeed.
					return etcdEncryptionKeyRotationPreconditionError("etcd key rotation failed in prepare; start a new rotation with a new workflow ID")
				}
				if status.State == db.EtcdKeyRotationStateFailed {
					return etcdEncryptionKeyRotationPreconditionError("failed etcd key rotation requires reset at its failed phase")
				}
				if status.State == db.EtcdKeyRotationStateRunning && ownsEtcdKeyRotationLock(&cluster) {
					return nil // retry after a committed begin; recovery resets the failed phase
				}
				return etcdEncryptionKeyRotationPreconditionError("etcd key rotation no longer owns the mutation lock")
			}
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
		if cluster.Conditions.Provisioning.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("cluster provisioning is unfinished")
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
		if cluster.NodeAgentUpgrade != nil {
			return etcdEncryptionKeyRotationPreconditionError("cannot rotate the etcd encryption key while a node-agent installation is in progress")
		}

		kubernetesVersion, err := version.ParseGeneric(cluster.KubernetesVersion)
		if err != nil || kubernetesVersion.LessThan(version.MustParseGeneric("1.34.0")) {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation requires Kubernetes 1.34 or newer")
		}

		nodes, err := stableRotationControlPlane(ctx, tx, cluster.ID)
		if err != nil {
			return err
		}
		if err := requireControlPlaneNodesAtClusterKubernetesVersion(ctx, tx, nodes, cluster.KubernetesVersion); err != nil {
			return err
		}

		op := db.OperationTypeEtcdEncryption // legacy workflow histories
		now := time.Now().UTC()
		if input.WorkflowID != "" {
			op = db.OperationTypeEtcdKeyRotation
			cluster.Conditions.EtcdKeyRotation = &db.EtcdKeyRotationStatus{
				WorkflowID: input.WorkflowID, State: db.EtcdKeyRotationStateRunning,
				Phase: db.EtcdKeyRotationPhasePrepare, StartedAt: now, LastProgressAt: now,
			}
		}
		db.ClearUpdateFailed(&cluster)
		db.SetUpdateInProgress(
			&cluster,
			op,
			"rotating etcd encryption key",
			now,
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

// requireControlPlaneNodesAtClusterKubernetesVersion refuses rotation while a
// control-plane Kubernetes upgrade has not reached every node: the desired
// cluster version is set before ControlPlaneUpgrading, so nodes may still run
// a version without the encryption-config reload metric. It compares pinned
// snapshot versions as controlPlaneControllerNeedsK8sUpgrade does.
func requireControlPlaneNodesAtClusterKubernetesVersion(
	ctx context.Context,
	tx db.Tx,
	nodes []db.Node,
	kubernetesVersion string,
) error {
	ids := make([]uuid.UUID, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ControlPlaneConfigurationID)
	}
	rows, err := tx.ListControlPlaneConfigurationsByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("failed to list control-plane node configurations: %w", err)
	}
	pinned := make(map[uuid.UUID]string, len(rows))
	for _, row := range rows {
		pinned[row.ID] = row.Config.KubernetesVersion
	}
	for _, node := range nodes {
		nodeVersion, ok := pinned[node.ControlPlaneConfigurationID]
		if node.ControlPlaneConfigurationID == uuid.Nil || !ok {
			return etcdEncryptionKeyRotationPreconditionError(
				fmt.Sprintf("cannot determine the Kubernetes version of control-plane node %s", node.Name),
			)
		}
		if nodeVersion != kubernetesVersion {
			return etcdEncryptionKeyRotationPreconditionError(fmt.Sprintf(
				"control-plane node %s runs Kubernetes %s, not the cluster's %s: a control-plane Kubernetes upgrade has not finished rolling out; wait for it before rotating the etcd encryption key",
				node.Name, nodeVersion, kubernetesVersion,
			))
		}
	}
	return nil
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
	if err != nil {
		// Identity diagnostics are fixed text and tell the operator what to fix.
		var identityErr encryptionIdentityError
		if errors.As(err, &identityErr) {
			return identityErr
		}
		// Kubernetes validation errors may contain the rejected Secret body.
		var applicationErr *temporal.ApplicationError
		if errors.As(err, &applicationErr) && applicationErr.NonRetryable() {
			return etcdEncryptionKeyRotationPreconditionError("etcd rotation key prerequisites are not satisfied")
		}
		return errors.New("preparing etcd rotation key failed; inspect Kubernetes access and key state")
	}
	return nil
}

type CompleteEtcdEncryptionKeyRotationInput struct {
	ClusterID  uuid.UUID
	WorkflowID string `json:"WorkflowID,omitempty"`
}

type CompleteEtcdEncryptionKeyRotationResult struct{}

func (a *Activities) CompleteEtcdEncryptionKeyRotation(
	ctx context.Context,
	input CompleteEtcdEncryptionKeyRotationInput,
) (CompleteEtcdEncryptionKeyRotationResult, error) {
	var err error
	if input.WorkflowID == "" {
		err = a.completeEtcdEncryptionOperation(ctx, input.ClusterID, false)
	} else {
		err = a.completeEtcdKeyRotation(ctx, input.ClusterID, input.WorkflowID)
	}
	if err != nil {
		return CompleteEtcdEncryptionKeyRotationResult{}, err
	}
	return CompleteEtcdEncryptionKeyRotationResult{}, nil
}
