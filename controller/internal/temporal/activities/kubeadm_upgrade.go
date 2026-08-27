// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type KubeadmUpgradeApplyInput struct {
	ClusterID         uuid.UUID
	KubernetesVersion string
}

type KubeadmUpgradeApplyResult struct {
	NodeID   uuid.UUID
	NodeName string
	Output   string
}

// KubeadmUpgradeApply runs kubeadm upgrade apply through the node agent on a
// healthy joined control-plane node, falling back to any joined control-plane
// node when none currently has healthy conditions.
func (a *Activities) KubeadmUpgradeApply(
	ctx context.Context,
	input KubeadmUpgradeApplyInput,
) (KubeadmUpgradeApplyResult, error) {
	controlPlaneNodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &input.ClusterID,
		ControlPlane: pointerTo(true),
	})
	if err != nil {
		return KubeadmUpgradeApplyResult{}, fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	candidates, healthyCandidates := nodes.PickControlPlaneRunbookCandidates(controlPlaneNodes)
	if len(candidates) == 0 {
		return KubeadmUpgradeApplyResult{}, fmt.Errorf("no joined control plane nodes are available for kubeadm upgrade")
	}
	if !healthyCandidates {
		activity.GetLogger(ctx).Warn(
			"No healthy joined control plane nodes; falling back to all joined nodes for kubeadm upgrade",
			"clusterID", input.ClusterID,
			"candidateCount", len(candidates),
		)
	}

	//nolint:gosec // Non-secure random number generator is acceptable here.
	controlPlaneNode := candidates[rand.Intn(len(candidates))]
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	response, err := a.nodeAgentClient.KubeadmUpgradeApply(ctx, controlPlaneNode, &nkeproto.KubeadmUpgradeApplyRequest{
		TargetVersion: input.KubernetesVersion,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			activity.GetLogger(ctx).Info(
				"KubeadmUpgradeApply is unavailable; falling back to legacy kubeadm upgrade runbook",
				"nodeID", controlPlaneNode.ID,
				"nodeName", controlPlaneNode.Name,
			)
			result, runbookErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
				HostID:      controlPlaneNode.ID,
				RunbookName: "upgrade-kubelet-kubeadm",
				RunbookEnv: map[string]string{
					"VERSION":              input.KubernetesVersion,
					"IS_MASTER":            trueValue,
					"KUBEADM_UPGRADE_ONLY": trueValue,
				},
			})
			if runbookErr != nil {
				return KubeadmUpgradeApplyResult{}, fmt.Errorf("failed to execute legacy kubeadm upgrade runbook on control plane node %s: %w", controlPlaneNode.Name, runbookErr)
			}

			return KubeadmUpgradeApplyResult{
				NodeID:   controlPlaneNode.ID,
				NodeName: controlPlaneNode.Name,
				Output:   result.Output,
			}, nil
		}
		return KubeadmUpgradeApplyResult{}, fmt.Errorf("failed to apply kubeadm upgrade on control plane node %s: %w", controlPlaneNode.Name, err)
	}
	if response.GetOutcome() != nkeproto.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED {
		return KubeadmUpgradeApplyResult{}, fmt.Errorf("kubeadm upgrade on control plane node %s returned outcome %s", controlPlaneNode.Name, response.GetOutcome())
	}

	return KubeadmUpgradeApplyResult{
		NodeID:   controlPlaneNode.ID,
		NodeName: controlPlaneNode.Name,
		Output:   joinCommandOutput(response.GetPlanOutput(), response.GetApplyOutput()),
	}, nil
}

func joinCommandOutput(outputs ...string) string {
	var output strings.Builder
	for _, commandOutput := range outputs {
		if commandOutput == "" {
			continue
		}
		if output.Len() > 0 && !strings.HasSuffix(output.String(), "\n") {
			output.WriteByte('\n')
		}
		output.WriteString(commandOutput)
	}
	return output.String()
}
