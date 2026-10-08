// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/runbook"
)

// stableRotationControlPlane rejects unfinished control-plane membership work
// before etcd key rotation takes ownership.
func stableRotationControlPlane(ctx context.Context, tx db.Tx, clusterID uuid.UUID) ([]db.Node, error) {
	return stableControlPlane(ctx, tx, clusterID, etcdEncryptionKeyRotationPreconditionError)
}

// ValidateEtcdRotationMembershipInput contains only the fixed node snapshot and
// rotation owner; no encryption key material is passed through Temporal.
type ValidateEtcdRotationMembershipInput struct {
	ClusterID       uuid.UUID
	WorkflowID      string
	ExpectedNodeIDs []uuid.UUID
}

// ValidateEtcdRotationMembership proves current DB and live etcd membership and
// requires every API server to be ready before destructive old-key removal.
func (a *Activities) ValidateEtcdRotationMembership(ctx context.Context, input ValidateEtcdRotationMembershipInput) error {
	var nodes []db.Node
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			return errors.New("cannot inspect rotation ownership")
		}
		status := cluster.Conditions.EtcdKeyRotation
		if !ownsEtcdKeyRotationLock(&cluster) || status == nil || status.WorkflowID != input.WorkflowID ||
			cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("rotation no longer owns the control plane")
		}
		nodes, err = stableRotationControlPlane(ctx, tx, input.ClusterID)
		if err != nil {
			return err
		}
		actual := make([]uuid.UUID, 0, len(nodes))
		for _, node := range nodes {
			actual = append(actual, node.ID)
		}
		expected := slices.Clone(input.ExpectedNodeIDs)
		slices.SortFunc(actual, compareNodeIDs)
		slices.SortFunc(expected, compareNodeIDs)
		if !slices.Equal(actual, expected) {
			return etcdEncryptionKeyRotationPreconditionError("control-plane membership changed during rotation")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(nodes) == 0 { // stableRotationControlPlane already rejects this; keep nodes[0] safe
		return etcdEncryptionKeyRotationPreconditionError("no control-plane nodes are available")
	}
	// Ownership is retained across these network calls. Admission writers check
	// that ownership under the same row lock; health observations remain enabled.
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Conditions.NodeAgentReady.Status != db.ConditionStatusTrue {
			return errors.New("every control-plane agent must be ready before old-key removal")
		}
		if _, ready := a.probeKubeReadyz(ctx, node); !ready {
			return errors.New("every control-plane API server must be ready before old-key removal")
		}
		names = append(names, node.Name)
	}
	script, err := runbook.Script("", "etcd-rotation-membership")
	if err != nil {
		return errors.New("cannot load rotation membership probe")
	}
	resp, err := a.nodeAgentClient.ExecuteScript(ctx, nodes[0], "etcd-rotation-membership", script, names)
	if err != nil || resp.GetExitCode() != 0 {
		// Never put arbitrary node output into workflow failure/history.
		return errors.New("live etcd membership or health does not match the rotation snapshot")
	}
	return nil
}

func compareNodeIDs(a, b uuid.UUID) int {
	return slices.Compare(a[:], b[:])
}
