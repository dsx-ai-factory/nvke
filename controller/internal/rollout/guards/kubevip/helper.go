// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubevip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/rollout"
)

func nodeAgentOperationDone(current *nkeproto.NodeAgentOperationStatus) (bool, error) {
	if current == nil {
		return false, errors.New("nil NodeAgentOperationStatus")
	}
	switch current.GetPhase() {
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED,
		nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
		return false, nil
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
		return true, nil
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		return false, fmt.Errorf("operation %s failed at step %q: %s",
			current.GetOperationId(), current.GetStep(), current.GetErrorMessage())
	default:
		return false, fmt.Errorf("operation %s returned unexpected phase %s",
			current.GetOperationId(), current.GetPhase())
	}
}

func operationIDSeed(ctx context.Context, cluster rollout.Cluster, node rollout.Node) string {
	if info, ok := activityInfo(ctx); ok {
		return fmt.Sprintf("%s/%s/%s/%s/%s/%s",
			cluster.ID(), node.ID(), GuardNameKubeVipDrainRestore,
			info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
	}
	// Direct, non-Temporal callers do not have a workflow/run/activity identity.
	// Use a fresh nonce so a later execution cannot reuse a completed agent
	// operation. The derived operation IDs are persisted in GuardState before
	// either Start RPC is dispatched.
	return fmt.Sprintf("%s/%s/%s/%s", cluster.ID(), node.ID(), GuardNameKubeVipDrainRestore, uuid.NewString())
}

func activityInfo(ctx context.Context) (activity.Info, bool) {
	var info activity.Info
	ok := false
	func() {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		info = activity.GetInfo(ctx)
		ok = info.WorkflowExecution.ID != "" || info.ActivityID != ""
	}()
	return info, ok
}

func operationID(kind, seed string) string {
	sum := sha256.Sum256([]byte(kind + "/" + seed))
	return kind + "-" + hex.EncodeToString(sum[:])[:20]
}
