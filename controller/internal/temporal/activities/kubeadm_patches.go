// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"

	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	legacyKubeadmPatchesRunbook = "kubeadm-patches"
)

// ensureKubeadmPatches stages the kubelet configuration patch through the
// legacy runbook when the connected node agent cannot consume request files.
func (a *Activities) ensureKubeadmPatches(ctx context.Context, node db.Node, nodePool db.NodePool) error {
	supported, err := a.nodeAgentClient.SupportsKubeadmFiles(ctx, node)
	if err != nil {
		return fmt.Errorf("checking node agent kubeadm file capability: %w", err)
	}
	if supported {
		return nil
	}

	activity.GetLogger(ctx).Info(
		"node agent does not support kubeadm request files; using legacy runbook",
		"nodeID", node.ID,
		"nodeName", node.Name,
		"runbookName", legacyKubeadmPatchesRunbook,
	)
	runbookEnv := map[string]string{}
	if nodePool.KubeReservedMemory != "" {
		runbookEnv["KUBE_RESERVED_MEM"] = nodePool.KubeReservedMemory
	}
	if nodePool.SystemReservedMemory != "" {
		runbookEnv["SYSTEM_RESERVED_MEM"] = nodePool.SystemReservedMemory
	}
	if _, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      node.ID,
		RunbookName: legacyKubeadmPatchesRunbook,
		RunbookEnv:  runbookEnv,
	}); err != nil {
		return fmt.Errorf("executing legacy kubeadm patches runbook: %w", err)
	}

	return nil
}
