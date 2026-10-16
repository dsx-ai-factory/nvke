// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	lostNodeProbeTimeout = 10 * time.Second
)

// RetireLostEtcdRotationNodeInput names the failed rotation owner and the
// control-plane node an operator declares permanently lost.
type RetireLostEtcdRotationNodeInput struct {
	ClusterID          uuid.UUID
	RotationWorkflowID string
	NodeID             uuid.UUID
}

// RetireLostEtcdRotationNodeResult reports whether the node row still needs teardown.
type RetireLostEtcdRotationNodeResult struct {
	TeardownRequired bool
}

// RetireLostEtcdRotationNode records, under the retained rotation lock, that a
// permanently lost control-plane node may be torn down. Only removal of that
// node passes the fence; joins stay blocked until the rotation completes on the
// survivors, so a replacement joins with the finalized key. A repeat is a no-op.
func (a *Activities) RetireLostEtcdRotationNode(ctx context.Context, input RetireLostEtcdRotationNodeInput) (RetireLostEtcdRotationNodeResult, error) {
	// The live probe runs before the row lock; the transaction rechecks everything else.
	agentAnswered := a.lostNodeAgentAnswers(ctx, input.NodeID)
	result := RetireLostEtcdRotationNodeResult{}
	phase := ""
	outcome, reason := credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			return errors.New("cannot inspect etcd key rotation ownership")
		}
		status := cluster.Conditions.EtcdKeyRotation
		if status != nil {
			phase = string(status.Phase)
		}
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return etcdEncryptionKeyRotationPreconditionError("cluster deletion is in progress; teardown removes every node")
		}
		if input.RotationWorkflowID == "" || !ownsEtcdKeyRotationLock(&cluster) || status == nil || status.WorkflowID != input.RotationWorkflowID {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation does not own the cluster mutation lock")
		}
		if status.RetiresNode(input.NodeID) {
			outcome, reason = credentialaudit.OutcomeNoop, credentialaudit.ReasonUnchanged
			_, err := tx.GetNode(ctx, input.NodeID)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return errors.New("cannot inspect retired control-plane node")
			}
			result.TeardownRequired = err == nil
			return nil
		}
		if status.State != db.EtcdKeyRotationStateFailed {
			return etcdEncryptionKeyRotationPreconditionError("etcd key rotation must have failed before a lost node is retired")
		}
		switch status.Phase {
		case db.EtcdKeyRotationPhaseAddSecondary, db.EtcdKeyRotationPhasePromotePrimary,
			db.EtcdKeyRotationPhaseRewrite, db.EtcdKeyRotationPhaseRemoveOldKeys:
		case db.EtcdKeyRotationPhasePrepare, db.EtcdKeyRotationPhaseFinalize:
			return etcdEncryptionKeyRotationPreconditionError("lost nodes are retired only in a failed node-mutation phase; reset finalize at its failed activity")
		default:
			return etcdEncryptionKeyRotationPreconditionError("invalid etcd key rotation phase")
		}
		node, err := lockRetirableNode(ctx, tx, cluster.ID, input.NodeID, agentAnswered)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		status.RetiredNodes = append(status.RetiredNodes, db.RetiredEtcdKeyRotationNode{NodeID: node.ID, RetiredAt: now, Phase: status.Phase})
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return errors.New("cannot persist retired etcd key rotation node")
		}
		db.SetCondition(&node.Conditions.Deleting, db.NewCondition(true, "Lost control-plane node retired from etcd key rotation"))
		if _, err := tx.UpdateNode(ctx, &node); err != nil {
			return errors.New("cannot mark retired control-plane node deleting")
		}
		outcome, reason = credentialaudit.OutcomeSuccess, credentialaudit.ReasonNodeRetired
		result.TeardownRequired = true
		return nil
	})
	event := credentialOperationAudit(input.ClusterID, &input.NodeID, credentialaudit.ActionRotate, credentialaudit.TypeEtcdKey, outcome, reason)
	event.Phase = phase
	a.recordCredentialAudit(ctx, event, input.RotationWorkflowID)
	if err != nil {
		return RetireLostEtcdRotationNodeResult{}, err
	}
	return result, nil
}

// lostNodeAgentAnswers reports whether the node agent still answers. Any
// answer means the host is not lost; errors and missing rows are left to the
// transaction to classify.
func (a *Activities) lostNodeAgentAnswers(ctx context.Context, nodeID uuid.UUID) bool {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil || node.IP == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, lostNodeProbeTimeout)
	defer cancel()
	_, err = a.nodeAgentClient.GetAgentVersion(probeCtx, node)
	return err == nil
}

// lockRetirableNode accepts one joined, unreachable control-plane node whose
// removal keeps a ready etcd quorum and does not overlap another removal.
func lockRetirableNode(ctx context.Context, tx db.Tx, clusterID, nodeID uuid.UUID, agentAnswered bool) (db.Node, error) {
	cp := true
	nodes, err := tx.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &cp})
	if err != nil {
		return db.Node{}, errors.New("cannot inspect control-plane membership")
	}
	members, ready := 0, 0
	for _, node := range nodes {
		if node.ID == nodeID || node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}
		if node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue || node.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return db.Node{}, etcdEncryptionKeyRotationPreconditionError("another control-plane removal is in flight; wait for it to finish")
		}
		members++
		if node.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue && node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue {
			ready++
		}
	}
	node, err := tx.GetNodeForUpdate(ctx, nodeID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return db.Node{}, errors.New("cannot inspect lost control-plane node")
	}
	if err != nil || node.ClusterID != clusterID || !node.ControlPlane || node.Conditions.Joined.Status != db.ConditionStatusTrue {
		return db.Node{}, etcdEncryptionKeyRotationPreconditionError("node is not a joined control-plane node of this cluster")
	}
	if node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue || node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return db.Node{}, etcdEncryptionKeyRotationPreconditionError("another control-plane removal is in flight; wait for it to finish")
	}
	if agentAnswered || node.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue ||
		node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue {
		return db.Node{}, etcdEncryptionKeyRotationPreconditionError("node is not lost: its agent or kubelet still reports ready; restore it and reset the rotation instead")
	}
	if ready < members/2+1 {
		return db.Node{}, etcdEncryptionKeyRotationPreconditionError("removing the node would leave no ready etcd quorum; restore etcd from backup")
	}
	return node, nil
}
