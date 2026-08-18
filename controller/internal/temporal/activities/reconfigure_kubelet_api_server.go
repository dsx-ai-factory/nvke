// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

type ReconfigureKubeletAPIServerInput struct {
	NodeID uuid.UUID
	Server string
}

type ReconfigureKubeletAPIServerResult struct{}

func (a *Activities) ReconfigureKubeletAPIServer(
	ctx context.Context,
	input ReconfigureKubeletAPIServerInput,
) (ReconfigureKubeletAPIServerResult, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ReconfigureKubeletAPIServerResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	response, err := a.nodeAgentClient.ReconfigureKubeletAPIServer(
		ctx,
		node,
		&nkeproto.ReconfigureKubeletAPIServerRequest{Server: input.Server},
	)
	if err != nil {
		return ReconfigureKubeletAPIServerResult{}, fmt.Errorf("failed to reconfigure kubelet API server on node %s: %w", node.Name, err)
	}
	if response == nil {
		return ReconfigureKubeletAPIServerResult{}, errors.New("node agent returned an empty kubelet API server response")
	}

	switch response.GetOutcome() {
	case nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_APPLIED,
		nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_UNCHANGED:
		return ReconfigureKubeletAPIServerResult{}, nil
	case nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_RESTORED:
		return ReconfigureKubeletAPIServerResult{}, fmt.Errorf("kubelet API server reconfiguration failed on node %s and the previous state was restored: %s", node.Name, response.GetErrorMessage())
	case nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED:
		return ReconfigureKubeletAPIServerResult{}, fmt.Errorf("kubelet API server reconfiguration failed on node %s and the previous state was not restored: %s", node.Name, response.GetErrorMessage())
	case nkeproto.ReconfigureKubeletAPIServerResponse_OUTCOME_UNSPECIFIED:
		return ReconfigureKubeletAPIServerResult{}, fmt.Errorf("node agent returned an unspecified kubelet API server outcome for node %s", node.Name)
	default:
		return ReconfigureKubeletAPIServerResult{}, fmt.Errorf("node agent returned unknown kubelet API server outcome %d for node %s", response.GetOutcome(), node.Name)
	}
}
