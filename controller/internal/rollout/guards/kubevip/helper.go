// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubevip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

const (
	nodeAgentOperationTypeKubeVipRestore = "kube_vip_restore"
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

func restoreOperationFromNodeInfo(
	resp *nkeproto.NodeInfoResponse,
	operationID string,
) (*nkeproto.NodeAgentOperationStatus, error) {
	if resp == nil {
		return nil, errors.New("node info response is nil")
	}
	if resp.GetJsonResponse() == "" {
		return nil, errors.New("node info json_response is empty")
	}

	var status nodeAgentStatus
	if err := json.Unmarshal([]byte(resp.GetJsonResponse()), &status); err != nil {
		return nil, fmt.Errorf("parse node info json_response: %w", err)
	}
	operation, ok := status.NodeAgentOperations[operationID]
	if !ok {
		return nil, fmt.Errorf("operation %s not found in node-agent status", operationID)
	}
	if operation.Type != nodeAgentOperationTypeKubeVipRestore {
		return nil, fmt.Errorf("operation %s has type %q, want %q",
			operationID, operation.Type, nodeAgentOperationTypeKubeVipRestore)
	}
	phase, err := nodeAgentOperationPhaseFromStatus(operation.Phase)
	if err != nil {
		return nil, fmt.Errorf("operation %s: %w", operationID, err)
	}
	if operation.ID == "" {
		operation.ID = operationID
	}

	return &nkeproto.NodeAgentOperationStatus{
		OperationId:  operation.ID,
		Phase:        phase,
		Step:         operation.Step,
		ErrorMessage: operation.ErrorMessage,
	}, nil
}

type nodeAgentStatus struct {
	NodeAgentOperations map[string]nodeAgentOperation `json:"nodeAgentOperations"`
}

type nodeAgentOperation struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Phase        string `json:"phase"`
	Step         string `json:"step"`
	ErrorMessage string `json:"errorMessage"`
}

func nodeAgentOperationPhaseFromStatus(phase string) (nkeproto.NodeAgentOperationPhase, error) {
	switch phase {
	case "running":
		return nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, nil
	case "succeeded":
		return nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, nil
	case "failed":
		return nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, nil
	default:
		return nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED,
			fmt.Errorf("unexpected phase %q", phase)
	}
}

func operationIDSeed(ctx context.Context, cluster rollout.Cluster, node rollout.Node) string {
	if info, ok := activityInfo(ctx); ok {
		return fmt.Sprintf("%s/%s/%s/%s/%s/%s",
			cluster.ID(), node.ID(), GuardNameKubeVipDrainRestore,
			info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
	}
	// Direct, non-Temporal callers do not have a workflow/run/activity identity.
	// Use a fresh nonce so a later execution cannot reuse a completed node-agent
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
