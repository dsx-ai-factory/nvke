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
	operationIDs []string
	err          error
}

func (l *recordingAgentUpgradeLauncher) Start(operationID string) error {
	l.operationIDs = append(l.operationIDs, operationID)
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
		OperationId:   "cluster-node-1",
		TargetVersion: "v0.1.2",
		Method:        pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT,
	}

	resp, err := server.StartNodeAgentUpgrade(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp.GetOperation())
	assert.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING, resp.GetOperation().GetPhase())
	assert.Equal(t, []string{"cluster-node-1"}, launcher.operationIDs)
	persisted, err := store.Load("cluster-node-1")
	require.NoError(t, err)
	assert.Equal(t, agentupgrade.PhasePending, persisted.Phase)
	assert.Equal(t, "v0.1.1", persisted.PreviousVersion)

	server.NodeStatus.AgentVersion = "v0.1.2"
	resp, err = server.StartNodeAgentUpgrade(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING, resp.GetOperation().GetPhase())
	assert.Equal(t, []string{"cluster-node-1", "cluster-node-1"}, launcher.operationIDs, "pending retry must reassert the idempotent systemd start")
}

func TestStartNodeAgentUpgradeRejectsNonCanonicalVersion(t *testing.T) {
	server := &NodeAgentServer{
		NodeStatus:           &NodeStatus{AgentVersion: "v0.1.1"},
		AgentUpgradeStore:    agentupgrade.NewStore(t.TempDir()),
		AgentUpgradeLauncher: &recordingAgentUpgradeLauncher{},
	}

	_, err := server.StartNodeAgentUpgrade(context.Background(), &pb.StartNodeAgentUpgradeRequest{
		OperationId:   "cluster-node-1",
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

func TestStartNodeAgentRollbackReferencesSuccessfulUpgrade(t *testing.T) {
	store := agentupgrade.NewStore(t.TempDir())
	_, _, err := store.Create(agentupgrade.Operation{
		ID:              "upgrade-1",
		Action:          agentupgrade.ActionUpgrade,
		TargetVersion:   "v0.1.2",
		PreviousVersion: "v0.1.1",
		Method:          agentupgrade.MethodAPT,
	})
	require.NoError(t, err)
	_, err = store.SetStatus("upgrade-1", agentupgrade.PhaseSucceeded, "verified", "")
	require.NoError(t, err)
	launcher := &recordingAgentUpgradeLauncher{}
	server := &NodeAgentServer{
		NodeStatus:           &NodeStatus{AgentVersion: "v0.1.2"},
		AgentUpgradeStore:    store,
		AgentUpgradeLauncher: launcher,
	}

	resp, err := server.StartNodeAgentRollback(context.Background(), &pb.StartNodeAgentRollbackRequest{
		OperationId:        "rollback-1",
		UpgradeOperationId: "upgrade-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "v0.1.1", resp.GetOperation().GetTargetVersion())
	assert.Equal(t, []string{"rollback-1"}, launcher.operationIDs)
	rollback, err := store.Load("rollback-1")
	require.NoError(t, err)
	assert.Equal(t, "upgrade-1", rollback.RestoreFromOperationID)

	_, err = server.StartNodeAgentRollback(context.Background(), &pb.StartNodeAgentRollbackRequest{
		OperationId:        "rollback-1",
		UpgradeOperationId: "upgrade-1",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"rollback-1", "rollback-1"}, launcher.operationIDs, "pending rollback retry must reassert the idempotent systemd start")
}
