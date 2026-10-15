// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/agentupgrade"
)

type recordingAgentUpgradeLauncher struct {
	requests []string
	err      error
}

func (l *recordingAgentUpgradeLauncher) Start(requestKey string) error {
	l.requests = append(l.requests, requestKey)
	return l.err
}

func TestStartNodeAgentUpgradePersistsBeforeLaunching(t *testing.T) {
	store := agentupgrade.NewStore(t.TempDir())
	launcher := &recordingAgentUpgradeLauncher{}
	server := &NodeAgentServer{
		NodeStatus:           &NodeStatus{AgentVersion: "v0.1.1"},
		AgentUpgradeStore:    store,
		AgentUpgradeLauncher: launcher,
	}
	req := &pb.StartNodeAgentUpgradeRequest{
		TargetVersion: "v0.1.2",
		Method:        pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT,
	}

	resp, err := server.StartNodeAgentUpgrade(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp.GetOperation())
	assert.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING, resp.GetOperation().GetPhase())
	require.Len(t, launcher.requests, 1)
	attempt := launcher.requests[0]
	persisted, err := store.Load(attempt)
	require.NoError(t, err)
	assert.Equal(t, agentupgrade.PhasePending, persisted.Phase)
	assert.Equal(t, "v0.1.1", persisted.PreviousVersion)

	server.NodeStatus.AgentVersion = "v0.1.2"
	resp, err = server.StartNodeAgentUpgrade(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING, resp.GetOperation().GetPhase())
	assert.Equal(t, []string{attempt, attempt}, launcher.requests, "pending retry must reassert the idempotent systemd start")
}

func TestStartNodeAgentUpgradeRejectsNonCanonicalVersion(t *testing.T) {
	server := &NodeAgentServer{
		NodeStatus:           &NodeStatus{AgentVersion: "v0.1.1"},
		AgentUpgradeStore:    agentupgrade.NewStore(t.TempDir()),
		AgentUpgradeLauncher: &recordingAgentUpgradeLauncher{},
	}

	_, err := server.StartNodeAgentUpgrade(context.Background(), &pb.StartNodeAgentUpgradeRequest{
		TargetVersion: "0.1.2",
		Method:        pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT,
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestInstallBundleRejectsNodeAgentReplacement(t *testing.T) {
	server := &NodeAgentServer{}
	_, err := server.InstallBundle(context.Background(), &pb.InstallBundleRequest{Name: "agent"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestGetNodeAgentUpgradeStatusReturnsEmptyResponseWithoutAnUpgrade(t *testing.T) {
	server := &NodeAgentServer{AgentUpgradeStore: agentupgrade.NewStore(t.TempDir())}
	response, err := server.GetNodeAgentUpgradeStatus(t.Context(), &pb.GetNodeAgentUpgradeStatusRequest{})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Nil(t, response.GetOperation())
}
