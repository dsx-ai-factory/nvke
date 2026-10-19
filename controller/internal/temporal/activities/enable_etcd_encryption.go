// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	nodeutil "github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

const (
	minEtcdEncryptionAgentVersion = "0.1.26"

	EtcdEncryptionOperationPhaseUnspecified = "unspecified"
	EtcdEncryptionOperationPhaseRunning     = "running"
	EtcdEncryptionOperationPhaseSucceeded   = "succeeded"
	EtcdEncryptionOperationPhaseFailed      = "failed"
)

type PrepareEtcdEncryptionInput struct {
	ClusterID uuid.UUID
}

type PrepareEtcdEncryptionResult struct {
	AlreadyEncrypted bool
	NodeIDs          []uuid.UUID
	ExecutorNodeID   uuid.UUID
}

func (a *Activities) PrepareEtcdEncryption(
	ctx context.Context,
	input PrepareEtcdEncryptionInput,
) (PrepareEtcdEncryptionResult, error) {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return PrepareEtcdEncryptionResult{}, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
		}
		return PrepareEtcdEncryptionResult{}, fmt.Errorf("failed to get cluster %s: %w", input.ClusterID, err)
	}
	if cluster.EtcdEncrypted {
		return PrepareEtcdEncryptionResult{AlreadyEncrypted: true}, nil
	}
	return a.prepareEtcdEncryptionControlPlaneNodes(ctx, input.ClusterID, "enable etcd encryption")
}

func (a *Activities) prepareEtcdEncryptionControlPlaneNodes(
	ctx context.Context,
	clusterID uuid.UUID,
	operation string,
) (PrepareEtcdEncryptionResult, error) {
	// The caller owns the cluster mutation lock before this runs. Load every
	// control-plane node here and fail before making changes if any node has not
	// joined.
	controlPlaneNodes, err := a.controlPlaneNodes(ctx, clusterID)
	if err != nil {
		return PrepareEtcdEncryptionResult{}, err
	}
	if len(controlPlaneNodes) == 0 {
		return PrepareEtcdEncryptionResult{}, fmt.Errorf("no control-plane nodes available for cluster %s", clusterID)
	}
	for _, node := range controlPlaneNodes {
		if node.IP == "" || node.Conditions.Joined.Status != db.ConditionStatusTrue {
			return PrepareEtcdEncryptionResult{}, fmt.Errorf("control-plane node %s is not joined; refusing to %s", node.Name, operation)
		}
	}

	executorCandidates, _ := nodeutil.PickControlPlaneRunbookCandidates(controlPlaneNodes)
	nodeIDs := make([]uuid.UUID, 0, len(controlPlaneNodes))
	for _, node := range controlPlaneNodes {
		nodeIDs = append(nodeIDs, node.ID)
	}
	return PrepareEtcdEncryptionResult{
		NodeIDs:        nodeIDs,
		ExecutorNodeID: executorCandidates[0].ID,
	}, nil
}

type ValidateEtcdEncryptionHealthInput struct {
	ClusterID       uuid.UUID
	ExpectedNodeIDs []uuid.UUID
}

func (a *Activities) ValidateEtcdEncryptionHealth(ctx context.Context, input ValidateEtcdEncryptionHealthInput) error {
	nodes, err := a.controlPlaneNodes(ctx, input.ClusterID)
	if err != nil {
		return err
	}
	if len(nodes) != len(input.ExpectedNodeIDs) {
		return fmt.Errorf("expected %d control-plane nodes, found %d", len(input.ExpectedNodeIDs), len(nodes))
	}
	for i, node := range nodes {
		if node.ID != input.ExpectedNodeIDs[i] {
			return fmt.Errorf("control-plane node set changed during etcd encryption")
		}
		if node.IP == "" || node.Conditions.Joined.Status != db.ConditionStatusTrue ||
			node.Conditions.NodeAgentReady.Status != db.ConditionStatusTrue ||
			node.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return fmt.Errorf("control-plane node %s is not healthy for etcd encryption", node.Name)
		}
		agentVersion, err := a.nodeAgentClient.GetAgentVersion(ctx, node)
		if err != nil {
			return fmt.Errorf("node agent is not ready on control-plane node %s: %w", node.Name, err)
		}
		if !nodeAgentVersionAtLeast(agentVersion, minEtcdEncryptionAgentVersion, false) {
			return fmt.Errorf(
				"node agent version %q on control-plane node %s is older than required version %s",
				agentVersion,
				node.Name,
				minEtcdEncryptionAgentVersion,
			)
		}
	}
	// Active kube-apiserver health gate — faithful port of SWE's KubeAPIHealthy
	// preflight. The DB KubernetesReady condition is kubelet-derived and does not
	// reflect apiserver readiness, so probe /readyz on each control-plane node's own
	// apiserver and require a quorum-plus-one to be healthy before any disruptive step.
	if err := a.probeControlPlaneAPIHealth(ctx, nodes); err != nil {
		return err
	}
	return nil
}

type EtcdEncryptionClusterInput struct {
	ClusterID uuid.UUID
}

func (a *Activities) EnsurePendingEtcdEncryptionKey(ctx context.Context, input EtcdEncryptionClusterInput) error {
	_, err := a.ensurePendingKubeAPIEncryptionKey(ctx, input.ClusterID)
	return err
}

// FinalizePendingEtcdEncryptionKey also serves rotation histories recorded
// before rotation ownership. Enablement finalizes before EtcdEncrypted is set,
// and rotation begins only after it is set, so the flag selects the protocol.
func (a *Activities) FinalizePendingEtcdEncryptionKey(ctx context.Context, input EtcdEncryptionClusterInput) error {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster %s: %w", input.ClusterID, err)
	}
	return a.finalizePendingKubeAPIEncryptionKey(ctx, input.ClusterID, !cluster.EtcdEncrypted)
}

type EtcdEncryptionRewriteInput struct {
	NodeID      uuid.UUID
	OperationID string
}

type EtcdEncryptionRewriteOperation struct {
	OperationID  string
	Phase        string
	ErrorMessage string
}

func (a *Activities) StartEtcdEncryptionRewrite(
	ctx context.Context,
	input EtcdEncryptionRewriteInput,
) (EtcdEncryptionRewriteOperation, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return EtcdEncryptionRewriteOperation{}, fmt.Errorf("failed to get rewrite node %s: %w", input.NodeID, err)
	}

	// A Temporal retry may arrive after agent accepted Start but before the controller
	// received the response. Probe the intended operation ID first and resume it
	// when it is already recorded, instead of treating a missing response as a
	// fresh start that the agent would reject.
	current, err := a.nodeAgentClient.GetEncryptedObjectsRewriteStatus(ctx, node, input.OperationID)
	if err == nil {
		op := etcdEncryptionRewriteOperationFromProto(current)
		if op.OperationID == input.OperationID {
			switch op.Phase {
			case EtcdEncryptionOperationPhaseRunning:
				return a.startEtcdEncryptionRewrite(ctx, node, input.OperationID)
			case EtcdEncryptionOperationPhaseSucceeded, EtcdEncryptionOperationPhaseFailed:
				return op, nil
			default:
				return EtcdEncryptionRewriteOperation{}, fmt.Errorf(
					"encrypted-object rewrite %s has unknown phase %q",
					op.OperationID,
					op.Phase,
				)
			}
		}
	}

	return a.startEtcdEncryptionRewrite(ctx, node, input.OperationID)
}

func (a *Activities) GetEtcdEncryptionRewriteStatus(
	ctx context.Context,
	input EtcdEncryptionRewriteInput,
) (EtcdEncryptionRewriteOperation, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return EtcdEncryptionRewriteOperation{}, fmt.Errorf("failed to get rewrite node %s: %w", input.NodeID, err)
	}
	operation, err := a.nodeAgentClient.GetEncryptedObjectsRewriteStatus(ctx, node, input.OperationID)
	if err != nil {
		return EtcdEncryptionRewriteOperation{}, errors.New("encrypted-object rewrite request failed; inspect node rewrite state")
	}
	op, err := etcdEncryptionRewriteOperationForID(operation, input.OperationID)
	if err != nil {
		return EtcdEncryptionRewriteOperation{}, err
	}
	if op.Phase == EtcdEncryptionOperationPhaseRunning {
		// The acknowledgement is durable, but the agent's rewrite goroutine is
		// not. Reissuing the same logical Start is a no-op while active and
		// resumes a persisted RUNNING operation after an agent process restart.
		// Keep this inside the existing activity so historical workflow command
		// sequences and the acknowledged operation ID remain unchanged.
		return a.startEtcdEncryptionRewrite(ctx, node, input.OperationID)
	}
	return op, nil
}

func (a *Activities) startEtcdEncryptionRewrite(
	ctx context.Context,
	node db.Node,
	operationID string,
) (EtcdEncryptionRewriteOperation, error) {
	operation, err := a.nodeAgentClient.StartEncryptedObjectsRewrite(ctx, node, operationID)
	if err != nil {
		return EtcdEncryptionRewriteOperation{}, errors.New("encrypted-object rewrite request failed; inspect node rewrite state")
	}
	return etcdEncryptionRewriteOperationForID(operation, operationID)
}

func etcdEncryptionRewriteOperationForID(
	operation *nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation,
	operationID string,
) (EtcdEncryptionRewriteOperation, error) {
	op := etcdEncryptionRewriteOperationFromProto(operation)
	if strings.TrimSpace(operationID) == "" || op.OperationID != operationID {
		return EtcdEncryptionRewriteOperation{}, errors.New("encrypted-object rewrite returned an unexpected operation; inspect node rewrite state")
	}
	switch op.Phase {
	case EtcdEncryptionOperationPhaseRunning, EtcdEncryptionOperationPhaseSucceeded, EtcdEncryptionOperationPhaseFailed:
		return op, nil
	default:
		return EtcdEncryptionRewriteOperation{}, errors.New("encrypted-object rewrite returned an unknown phase; inspect node rewrite state")
	}
}

func etcdEncryptionRewriteOperationFromProto(
	operation *nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation,
) EtcdEncryptionRewriteOperation {
	if operation == nil {
		return EtcdEncryptionRewriteOperation{}
	}
	phase := EtcdEncryptionOperationPhaseUnspecified
	switch operation.GetState() {
	case nvkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_UNSPECIFIED:
	case nvkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING:
		phase = EtcdEncryptionOperationPhaseRunning
	case nvkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED:
		phase = EtcdEncryptionOperationPhaseSucceeded
	case nvkeproto.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_FAILED:
		phase = EtcdEncryptionOperationPhaseFailed
	}
	return EtcdEncryptionRewriteOperation{
		OperationID:  operation.GetOperationId(),
		Phase:        phase,
		ErrorMessage: "", // upstream rewrite errors can echo rejected Secret bodies
	}
}

func (a *Activities) controlPlaneNodes(ctx context.Context, clusterID uuid.UUID) ([]db.Node, error) {
	controlPlane := true
	nodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list control-plane nodes for cluster %s: %w", clusterID, err)
	}
	slices.SortFunc(nodes, func(a, b db.Node) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	return nodes, nil
}

type CompleteEtcdEncryptionInput struct {
	ClusterID uuid.UUID
}

type CompleteEtcdEncryptionResult struct{}

func (a *Activities) CompleteEtcdEncryption(ctx context.Context, input CompleteEtcdEncryptionInput) (CompleteEtcdEncryptionResult, error) {
	err := a.completeEtcdEncryptionOperation(ctx, input.ClusterID, true)
	if err != nil {
		return CompleteEtcdEncryptionResult{}, err
	}
	return CompleteEtcdEncryptionResult{}, nil
}

func (a *Activities) completeEtcdEncryptionOperation(ctx context.Context, clusterID uuid.UUID, markEncrypted bool) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, clusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if markEncrypted {
			cluster.EtcdEncrypted = true
		}
		clearEtcdEncryptionUpdateInProgress(&cluster)
		db.ClearUpdateFailed(&cluster)

		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to update cluster after etcd encryption: %w", err)
		}

		return nil
	})
}

type ClearEtcdEncryptionUpdateInProgressInput struct {
	ClusterID      uuid.UUID
	FailureMessage string
}

type ClearEtcdEncryptionUpdateInProgressResult struct{}

func (a *Activities) ClearEtcdEncryptionUpdateInProgress(
	ctx context.Context,
	input ClearEtcdEncryptionUpdateInProgressInput,
) (ClearEtcdEncryptionUpdateInProgressResult, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if !clearEtcdEncryptionUpdateInProgress(&cluster) {
			return nil
		}
		if input.FailureMessage != "" {
			db.SetUpdateFailed(&cluster, input.FailureMessage)
		}

		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to clear etcd encryption update progress: %w", err)
		}

		return nil
	})
	if err != nil {
		return ClearEtcdEncryptionUpdateInProgressResult{}, err
	}

	return ClearEtcdEncryptionUpdateInProgressResult{}, nil
}

func clearEtcdEncryptionUpdateInProgress(cluster *db.Cluster) bool {
	if cluster.Conditions.UpdateInProgress == nil ||
		cluster.Conditions.UpdateInProgress.Type != db.OperationTypeEtcdEncryption {
		return false
	}
	db.ClearUpdateInProgress(cluster)
	return true
}
