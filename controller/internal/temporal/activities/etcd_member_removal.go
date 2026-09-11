// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"math/rand"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

const (
	legacyEtcdMemberRemoveRunbook = "etcd-member-remove"
)

// EtcdMemberRemovalInput identifies the member and a surviving node that may
// execute its removal.
type EtcdMemberRemovalInput struct {
	ClusterID        uuid.UUID
	TargetNodeID     uuid.UUID
	TargetMemberName string
}

// RemoveEtcdMember removes the target through a surviving control-plane node.
// Only an unsupported RPC selects the legacy runbook path.
func (a *Activities) RemoveEtcdMember(ctx context.Context, input EtcdMemberRemovalInput) error {
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	executor, err := a.pickEtcdMemberRemovalExecutor(ctx, input)
	if err != nil {
		return err
	}
	if _, err := a.nodeAgentClient.RemoveEtcdMember(ctx, executor, &nkeproto.RemoveEtcdMemberRequest{
		Target: input.TargetMemberName,
	}); err != nil {
		if status.Code(err) == codes.Unimplemented {
			activity.GetLogger(ctx).Info("RemoveEtcdMember is unavailable; falling back to legacy etcd member removal runbook",
				"nodeID", executor.ID, "nodeName", executor.Name)
			return a.removeEtcdMemberWithLegacyRunbook(ctx, input, executor)
		}
		return fmt.Errorf("failed to remove etcd member from control plane node %s: %w", executor.Name, err)
	}
	return nil
}

func (a *Activities) pickEtcdMemberRemovalExecutor(ctx context.Context, input EtcdMemberRemovalInput) (db.Node, error) {
	controlPlaneNodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &input.ClusterID,
		ControlPlane: pointerTo(true),
	})
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to list control plane nodes for etcd member removal: %w", err)
	}

	survivors := make([]db.Node, 0, len(controlPlaneNodes))
	for _, node := range controlPlaneNodes {
		if node.ID != input.TargetNodeID {
			survivors = append(survivors, node)
		}
	}
	candidates, healthyCandidates := nodes.PickControlPlaneRunbookCandidates(survivors)
	if len(candidates) == 0 {
		return db.Node{}, errors.New("no surviving joined control plane nodes are available for etcd member removal")
	}
	if !healthyCandidates {
		activity.GetLogger(ctx).Warn("No healthy surviving control plane nodes; falling back to all joined nodes for etcd member removal",
			"clusterID", input.ClusterID, "candidateCount", len(candidates))
	}

	//nolint:gosec // Non-secure random selection preserves cluster-runbook behavior.
	return candidates[rand.Intn(len(candidates))], nil
}

func (a *Activities) removeEtcdMemberWithLegacyRunbook(ctx context.Context, input EtcdMemberRemovalInput, executor db.Node) error {
	// RemoveEtcdMember permits the expected 2-to-1 transition. Keep old agents
	// behaviorally consistent by bypassing only the legacy runbook's additional
	// single-voter policy guard; its quorum-loss guard remains enforced.
	args := []string{input.TargetMemberName, "--allow-single-voter"}
	if _, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      executor.ID,
		RunbookName: legacyEtcdMemberRemoveRunbook,
		RunbookArgs: args,
	}); err != nil {
		return fmt.Errorf("failed to execute legacy etcd member removal runbook on node %s: %w", executor.Name, err)
	}
	return nil
}
