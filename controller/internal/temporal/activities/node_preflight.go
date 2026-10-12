// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	capdBootstrapSuccessPath           = "/run/cluster-api/bootstrap-success.complete"
	minCloudInitStatusPreflightVersion = "0.1.24"
	nodePreflightStatusReady           = "ready"
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
			return struct{}{}, a.nodePreflight(ctx, input.NodeID, activity.GetLogger(ctx))
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) nodePreflight(ctx context.Context, nodeID uuid.UUID, logger log.Logger) error {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	agentVersion, err := a.nodeAgentClient.GetAgentVersion(ctx, node)
	if err != nil {
		return fmt.Errorf("node agent is not ready on node %s: %w", node.Name, err)
	}

	// agent versions before v0.1.24 do not have nvkeproto.CheckCloudInitStatus.
	// So the only preflight gate was on agent connectivity.
	if !nodeAgentVersionAtLeast(agentVersion, minCloudInitStatusPreflightVersion, false) {
		logger.Info("node agent version predates strict node preflight checks", "nodeID", node.ID, "nodeName", node.Name, "agentVersion", agentVersion)
		return nil
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

	if preflightStatus != nodePreflightStatusReady {
		return fmt.Errorf("node preflight status %q: %s", resp.GetStatus(), resp.GetErrorMessage())
	}

	return nil
}

func nodeAgentVersionAtLeast(version, minVersion string, defaultOnParseError bool) bool {
	current, err := semver.ParseTolerant(normalizeNodeAgentVersion(version))
	if err != nil {
		return defaultOnParseError
	}
	minimum, err := semver.ParseTolerant(minVersion)
	if err != nil {
		return defaultOnParseError
	}
	return current.GTE(minimum)
}

func normalizeNodeAgentVersion(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, "-")
	if len(parts) == 2 && parts[1] == "dirty" {
		return parts[0]
	}
	if len(parts) >= 3 {
		if _, err := strconv.Atoi(parts[1]); err == nil &&
			strings.HasPrefix(parts[2], "g") &&
			isHexString(strings.TrimPrefix(parts[2], "g")) {
			return parts[0]
		}
	}
	return version
}

func isHexString(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
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
