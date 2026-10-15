// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/runbook"
)

// ConvergeNodeAgent executes one durable convergence step with a fresh connection.
func (c *NodeAgentClient) ConvergeNodeAgent(ctx context.Context, node db.Node, target, method string) (string, bool, error) {
	observation, err := c.GetKubeadmStatus(ctx, node)
	if err != nil {
		return "", false, err
	}
	version := observation.GetAgentVersion()
	if method != "apt" && method != "bundle" {
		return version, false, fmt.Errorf("node-agent update method must be apt or bundle")
	}
	// Inspect active status even on a version match. Installation or restoration
	// can still be running when a new agent first accepts connections.
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return version, false, err
	}
	defer cleanup()
	rpc := pb.NewNodeAgentClient(conn)
	current, err := rpc.GetNodeAgentUpgradeStatus(ctx, &pb.GetNodeAgentUpgradeStatusRequest{})
	op := current.GetOperation()
	upgradeActive := op.GetPhase() == pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING ||
		op.GetPhase() == pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING

	// No installation is active, and no version change is needed.
	if err == nil && !upgradeActive && (target == "" || version == target) {
		return version, true, nil
	}

	// Start the desired installation, including a retry after failure.
	if err == nil && !upgradeActive {
		err := startNodeAgentUpgrade(ctx, rpc, &pb.StartNodeAgentUpgradeRequest{
			TargetVersion: target,
			Method:        upgradeMethodFromName(method),
		})
		return version, false, err
	}

	// Keep the accepted installation running until it finishes.
	if err == nil && upgradeActive {
		if target != "" && op.GetTargetVersion() != target {
			return version, false, fmt.Errorf("another node-agent target is still active")
		}
		err := startNodeAgentUpgrade(ctx, rpc, &pb.StartNodeAgentUpgradeRequest{
			TargetVersion: op.GetTargetVersion(),
			Method:        op.GetMethod(),
		})
		return version, false, err
	}

	// Older agents use the bootstrap script.
	if status.Code(err) == codes.Unimplemented {
		return c.convergeNodeAgentFallback(ctx, node, target, method, version)
	}
	return version, false, fmt.Errorf("reading node-agent upgrade status: %w", err)
}

// startNodeAgentUpgrade separates request errors from installation failures.
func startNodeAgentUpgrade(ctx context.Context, rpc pb.NodeAgentClient, request *pb.StartNodeAgentUpgradeRequest) error {
	response, err := rpc.StartNodeAgentUpgrade(ctx, request)
	if err != nil {
		return fmt.Errorf("sending node-agent upgrade request: %w", err)
	}
	op := response.GetOperation()
	switch op.GetPhase() {
	case pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING,
		pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING,
		pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_SUCCEEDED:
		return nil
	case pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED,
		pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_NOT_RESTORED:
		return fmt.Errorf("node-agent installation failed (%s): %s", op.GetPhase(), op.GetErrorMessage())
	case pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_UNSPECIFIED:
	}
	return fmt.Errorf("unexpected node-agent upgrade phase: %s", op.GetPhase())
}

func (c *NodeAgentClient) convergeNodeAgentFallback(ctx context.Context, node db.Node, target, method, version string) (string, bool, error) {
	script, err := runbook.Script("", "node-agent-upgrade")
	if err != nil {
		return version, false, err
	}
	response, err := c.ExecuteScript(ctx, node, "node-agent-upgrade", script, []string{target, method, version})
	if err != nil {
		return version, false, err
	}
	if response.GetExitCode() != 0 {
		return version, false, fmt.Errorf("node-agent upgrade fallback failed: %s", response.GetErrorMessage())
	}
	var result struct {
		Phase        string `json:"phase"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal([]byte(response.GetOutput()), &result); err != nil {
		return version, false, fmt.Errorf("reading fallback upgrade status: %w", err)
	}
	if result.ErrorMessage != "" {
		return version, false, fmt.Errorf("node-agent upgrade: %s", result.ErrorMessage)
	}
	return version, (target == "" || version == target) && result.Phase == "Succeeded", nil
}

func upgradeMethodFromName(method string) pb.NodeAgentUpgradeMethod {
	if method == "bundle" {
		return pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_BUNDLE
	}
	return pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT
}
