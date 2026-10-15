// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/agentupgrade"
)

func TestUpgradeResumesAndRetriesAcrossRestart(t *testing.T) {
	root := t.TempDir()
	launcher := &recordingAgentUpgradeLauncher{}
	server := &NodeAgentServer{NodeStatus: &NodeStatus{AgentVersion: "v0.1.2"}, AgentUpgradeStore: agentupgrade.NewStore(root), AgentUpgradeLauncher: launcher}
	request := &pb.StartNodeAgentUpgradeRequest{TargetVersion: "v0.1.1", Method: pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT}
	first, err := server.StartNodeAgentUpgrade(context.Background(), request)
	require.NoError(t, err)
	require.NotNil(t, first.GetOperation())
	key := launcher.requests[0]
	require.NotEmpty(t, key)
	server.AgentUpgradeStore = agentupgrade.NewStore(root)
	resumed, err := server.StartNodeAgentUpgrade(context.Background(), request)
	require.NoError(t, err)
	require.NotNil(t, resumed.GetOperation())
	require.Equal(t, key, launcher.requests[1])
	_, err = server.AgentUpgradeStore.SetStatus(key, agentupgrade.PhaseFailedRestored, "failure", "artifact missing")
	require.NoError(t, err)
	observed, err := server.GetNodeAgentUpgradeStatus(context.Background(), &pb.GetNodeAgentUpgradeStatusRequest{})
	require.NoError(t, err)
	require.Equal(t, "artifact missing", observed.GetOperation().GetErrorMessage())
	retried, err := server.StartNodeAgentUpgrade(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, key, launcher.requests[len(launcher.requests)-1])
	require.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING, retried.GetOperation().GetPhase())
	request.TargetVersion = "v0.1.3"
	_, err = server.StartNodeAgentUpgrade(context.Background(), request)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestUpgradeRelaunchFailurePreservesRestorationUncertainty(t *testing.T) {
	for _, phase := range []agentupgrade.Phase{agentupgrade.PhasePending, agentupgrade.PhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			store := agentupgrade.NewStore(t.TempDir())
			_, _, err := store.Create(agentupgrade.Operation{TargetVersion: "v0.1.2", PreviousVersion: "v0.1.1", Method: agentupgrade.MethodAPT})
			require.NoError(t, err)
			_, err = store.SetStatus("v0.1.2_apt", phase, "installing", "")
			require.NoError(t, err)
			launcher := &recordingAgentUpgradeLauncher{err: errors.New("systemd unit unavailable")}
			server := &NodeAgentServer{NodeStatus: &NodeStatus{AgentVersion: "v0.1.1"}, AgentUpgradeStore: store, AgentUpgradeLauncher: launcher}
			response, err := server.StartNodeAgentUpgrade(t.Context(), &pb.StartNodeAgentUpgradeRequest{TargetVersion: "v0.1.2", Method: pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT})
			require.NoError(t, err)
			want := pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_NOT_RESTORED
			if phase == agentupgrade.PhasePending {
				want = pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED
			}
			require.Equal(t, want, response.GetOperation().GetPhase())
			require.Equal(t, "systemd unit unavailable", response.GetOperation().GetErrorMessage())
			persisted, err := store.Load("v0.1.2_apt")
			require.NoError(t, err)
			require.Equal(t, response.GetOperation().GetErrorMessage(), persisted.ErrorMessage)
		})
	}
}

func TestUpgradeAndDowngradeUseSameRPCAndStatus(t *testing.T) {
	server := &NodeAgentServer{NodeStatus: &NodeStatus{AgentVersion: "v0.1.1"}, AgentUpgradeStore: agentupgrade.NewStore(t.TempDir()), AgentUpgradeLauncher: &recordingAgentUpgradeLauncher{}}
	for _, target := range []string{"v0.1.2", "v0.1.1"} {
		t.Run(target, func(t *testing.T) {
			previous := server.NodeStatus.AgentVersion
			response, err := server.StartNodeAgentUpgrade(t.Context(), &pb.StartNodeAgentUpgradeRequest{TargetVersion: target, Method: pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT})
			require.NoError(t, err)
			require.Equal(t, target, response.GetOperation().GetTargetVersion())
			require.Equal(t, previous, response.GetOperation().GetPreviousVersion())
			observed, err := server.GetNodeAgentUpgradeStatus(t.Context(), &pb.GetNodeAgentUpgradeStatusRequest{})
			require.NoError(t, err)
			require.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING, observed.GetOperation().GetPhase())
			_, err = server.AgentUpgradeStore.SetStatus(target+"_apt", agentupgrade.PhaseSucceeded, "verified", "")
			require.NoError(t, err)
			server.NodeStatus.AgentVersion = target
			observed, err = server.GetNodeAgentUpgradeStatus(t.Context(), &pb.GetNodeAgentUpgradeStatusRequest{})
			require.NoError(t, err)
			require.Equal(t, target, observed.GetOperation().GetTargetVersion())
			require.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_SUCCEEDED, observed.GetOperation().GetPhase())
		})
	}
}
