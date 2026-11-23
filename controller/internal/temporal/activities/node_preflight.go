// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	capdBootstrapSuccessPath       = "/run/cluster-api/bootstrap-success.complete"
	nodePreflightStatusNotReady    = "notready"
	nodePreflightStatusReady       = "ready"
	nodePreflightNotReadyErrorType = "NodePreflightNotReady"
)

type NodePreflightInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

func (a *Activities) NodePreflight(ctx context.Context, input NodePreflightInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.NodeProvisioningConditionMessage,
		UpdateCondition: a.nodeProvisioningConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			return struct{}{}, a.nodePreflight(ctx, input.NodeID)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) nodePreflight(ctx context.Context, nodeID uuid.UUID) error {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Readiness normally populates AgentVersion, but health checks can be disabled.
	// In that case, discover the version for this request without persisting it.
	if strings.TrimSpace(node.AgentVersion) == "" {
		node.AgentVersion, err = a.nodeAgentClient.GetAgentVersion(ctx, node)
		if err != nil {
			return fmt.Errorf("failed to get agent version for node %s: %w", node.Name, err)
		}
	}
	if strings.TrimSpace(node.AgentVersion) == "" {
		return nodePreflightNotReadyError(
			fmt.Sprintf("node agent version is not populated on node %s", node.Name),
			nil,
		)
	}

	req, err := a.nodePreflightRequestForNode(ctx, node)
	if err != nil {
		return err
	}

	resp, err := a.nodeAgentClient.ExecuteNodePreflight(ctx, node, req)
	if err != nil {
		return err
	}
	preflightStatus := strings.ToLower(resp.GetStatus())
	if preflightStatus == nodePreflightStatusReady {
		return nil
	}

	message := fmt.Sprintf("node preflight status %q: %s", resp.GetStatus(), resp.GetErrorMessage())
	if preflightStatus == nodePreflightStatusNotReady {
		return nodePreflightNotReadyError(message, nil)
	}

	return errors.New(message)
}

func nodePreflightNotReadyError(message string, cause error) error {
	return temporal.NewApplicationErrorWithOptions(
		message,
		nodePreflightNotReadyErrorType,
		temporal.ApplicationErrorOptions{
			Cause:    cause,
			Category: temporal.ApplicationErrorCategoryBenign,
		},
	)
}

func (a *Activities) nodePreflightRequestForNode(ctx context.Context, node db.Node) (*nvkeproto.ExecuteNodePreflightRequest, error) {
	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	// Docker machine backed nodes do not have a cloud-init status, so we need to check for the CAPD bootstrap success sentinel instead.
	isDockerMachineBacked, err := a.isDockerMachineBackedNode(ctx, cluster, node)
	if err != nil {
		return nil, fmt.Errorf("failed to check if node is docker machine backed: %w", err)
	}
	if isDockerMachineBacked {
		return dockerMachineBackedNodePreflightRequest(), nil
	}

	// Normal non-docker machine backed nodes have a cloud-init status, so we can use the default preflight request.
	return &nvkeproto.ExecuteNodePreflightRequest{
		CheckCloudInitStatus: &nvkeproto.CheckCloudInitStatus{},
	}, nil
}

func dockerMachineBackedNodePreflightRequest() *nvkeproto.ExecuteNodePreflightRequest {
	return &nvkeproto.ExecuteNodePreflightRequest{
		CheckCloudInitStatus: nil,
		Checks: []*nvkeproto.NodePreflightCheck{
			{
				Name: "cluster-api-bootstrap-success",
				Check: &nvkeproto.NodePreflightCheck_FileExists{
					FileExists: &nvkeproto.FileExistsPreflightCheck{Path: capdBootstrapSuccessPath},
				},
			},
		},
	}
}
