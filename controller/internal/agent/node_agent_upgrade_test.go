// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type upgradeRPCServer struct {
	pb.UnimplementedNodeAgentServer
	mu             sync.Mutex
	version        string
	typed          bool
	operation      *pb.NodeAgentUpgradeOperation
	starts         int
	script         *pb.ExecuteScriptRequest
	fallback       string
	statusError    error
	startError     error
	startOperation *pb.NodeAgentUpgradeOperation
}

func (s *upgradeRPCServer) GetKubeadmStatus(context.Context, *pb.KubeadmStatusRequest) (*pb.KubeadmStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.KubeadmStatusResponse{AgentVersion: s.version}, nil
}

func (s *upgradeRPCServer) GetNodeAgentUpgradeStatus(_ context.Context, _ *pb.GetNodeAgentUpgradeStatusRequest) (*pb.GetNodeAgentUpgradeStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.statusError != nil {
		return nil, s.statusError
	}
	if !s.typed {
		return nil, status.Error(codes.Unimplemented, "upgrade RPC unavailable")
	}
	if s.operation == nil {
		return &pb.GetNodeAgentUpgradeStatusResponse{}, nil
	}
	operation := proto.Clone(s.operation).(*pb.NodeAgentUpgradeOperation)
	return &pb.GetNodeAgentUpgradeStatusResponse{Operation: operation}, nil
}

func (s *upgradeRPCServer) StartNodeAgentUpgrade(_ context.Context, request *pb.StartNodeAgentUpgradeRequest) (*pb.StartNodeAgentUpgradeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.starts++
	if s.startError != nil {
		return nil, s.startError
	}
	if s.startOperation != nil {
		s.operation = proto.Clone(s.startOperation).(*pb.NodeAgentUpgradeOperation)
		return &pb.StartNodeAgentUpgradeResponse{Operation: s.operation}, nil
	}
	s.operation = &pb.NodeAgentUpgradeOperation{TargetVersion: request.GetTargetVersion(), Method: request.GetMethod(), Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING}
	return &pb.StartNodeAgentUpgradeResponse{Operation: s.operation}, nil
}

func (s *upgradeRPCServer) ExecuteScript(_ context.Context, request *pb.ExecuteScriptRequest) (*pb.ExecuteScriptResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = request
	return &pb.ExecuteScriptResponse{Output: s.fallback}, nil
}

func upgradeTestClient(t *testing.T, server *upgradeRPCServer) (*NodeAgentClient, db.Node) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rpc := grpc.NewServer()
	pb.RegisterNodeAgentServer(rpc, server)
	go func() { _ = rpc.Serve(listener) }()
	t.Cleanup(rpc.Stop)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	transport := &Transport{cfg: config.Config{NodeAgent: config.NodeAgentConfig{Port: port}}}
	return NewClientWithTransport(transport), db.Node{IP: host}
}

func TestConvergeAgentUsesTypedRequests(t *testing.T) {
	server := &upgradeRPCServer{version: "v0.1.2", typed: true}
	client, node := upgradeTestClient(t, server)
	version, done, err := client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "apt")
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, "v0.1.2", version)
	require.Equal(t, 1, server.starts)
	server.mu.Lock()
	server.operation.Phase = pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED
	server.operation.ErrorMessage = "previous installation failed"
	server.mu.Unlock()
	_, done, err = client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "apt")
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, 2, server.starts)
	server.mu.Lock()
	server.version = "v0.1.1"
	server.operation.Phase = pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING
	server.mu.Unlock()
	_, done, err = client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "apt")
	require.NoError(t, err)
	require.False(t, done, "matching version alone must not complete an active installer")
	server.mu.Lock()
	server.operation.Phase = pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_SUCCEEDED
	server.mu.Unlock()
	_, done, err = client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "apt")
	require.NoError(t, err)
	require.True(t, done)
}

func TestConvergeAgentFallbackUsesDurableStatusAndExactObservation(t *testing.T) {
	server := &upgradeRPCServer{version: "v0.1.2", fallback: `{"phase":"Succeeded","errorMessage":""}`}
	client, node := upgradeTestClient(t, server)
	_, done, err := client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "bundle")
	require.NoError(t, err)
	require.False(t, done)
	require.Zero(t, server.starts)
	require.Contains(t, server.script.GetScriptContents(), "systemd-run")
	require.False(t, server.script.GetAsync())
	require.Equal(t, []string{"v0.1.1", "bundle", "v0.1.2"}, server.script.GetArgs())
	server.mu.Lock()
	server.version = "v0.1.1"
	server.fallback = `{"phase":"Running","errorMessage":""}`
	server.mu.Unlock()
	_, done, err = client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "bundle")
	require.NoError(t, err)
	require.False(t, done)
	server.mu.Lock()
	server.fallback = `{"phase":"Succeeded","errorMessage":""}`
	server.mu.Unlock()
	_, done, err = client.ConvergeNodeAgent(context.Background(), node, "v0.1.1", "bundle")
	require.NoError(t, err)
	require.True(t, done)
}

func TestConvergeAgentDrainDoesNotStartNewTarget(t *testing.T) {
	for _, typed := range []bool{true, false} {
		t.Run(fmt.Sprint(typed), func(t *testing.T) {
			server := &upgradeRPCServer{version: "v0.1.1", typed: typed, fallback: `{"phase":"Succeeded","errorMessage":""}`}
			client, node := upgradeTestClient(t, server)
			_, done, err := client.ConvergeNodeAgent(context.Background(), node, "", "apt")
			require.NoError(t, err)
			require.True(t, done)
			require.Zero(t, server.starts)
		})
	}
}

func TestConvergeAgentDoesNotCarryFailureAcrossTargets(t *testing.T) {
	server := &upgradeRPCServer{version: "v0.1.1", typed: true, operation: &pb.NodeAgentUpgradeOperation{TargetVersion: "v0.1.2", Method: pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT, Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED, ErrorMessage: "old artifact missing"}}
	client, node := upgradeTestClient(t, server)
	_, done, err := client.ConvergeNodeAgent(context.Background(), node, "v0.1.3", "apt")
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, 1, server.starts)
	require.Equal(t, "v0.1.3", server.operation.GetTargetVersion())
}

func TestConvergeAgentDistinguishesProgressAndFailures(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, test := range []struct {
			name           string
			startOperation *pb.NodeAgentUpgradeOperation
			startError     error
			wantError      string
			wantCode       codes.Code
		}{
			{
				name: "accepted request is in progress",
			},
			{
				name:           "running response ignores an earlier error",
				startOperation: &pb.NodeAgentUpgradeOperation{Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING, ErrorMessage: "previous failure"},
			},
			{
				name:       "RPC failure preserves its status code",
				startError: status.Error(codes.Unavailable, "agent unreachable"),
				wantError:  "sending node-agent upgrade request:",
				wantCode:   codes.Unavailable,
			},
			{
				name:           "failed installation with restoration",
				startOperation: &pb.NodeAgentUpgradeOperation{Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED, ErrorMessage: "artifact missing"},
				wantError:      "node-agent installation failed (NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED): artifact missing",
			},
			{
				name:           "failed installation without a message",
				startOperation: &pb.NodeAgentUpgradeOperation{Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_NOT_RESTORED},
				wantError:      "node-agent installation failed (NODE_AGENT_UPGRADE_PHASE_FAILED_NOT_RESTORED)",
			},
			{
				name:           "missing response phase cannot imply acceptance",
				startOperation: &pb.NodeAgentUpgradeOperation{},
				wantError:      "unexpected node-agent upgrade phase:",
			},
		} {
			t.Run(fmt.Sprintf("active_%t/%s", active, test.name), func(t *testing.T) {
				server := &upgradeRPCServer{version: "v0.1.1", typed: true, startError: test.startError, startOperation: test.startOperation}
				if active {
					server.operation = &pb.NodeAgentUpgradeOperation{TargetVersion: "v0.1.2", Method: pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT, Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING}
				}
				client, node := upgradeTestClient(t, server)
				version, done, err := client.ConvergeNodeAgent(t.Context(), node, "v0.1.2", "apt")
				if test.wantError == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, test.wantError)
				}
				if test.wantCode != codes.OK {
					require.Equal(t, test.wantCode, status.Code(err))
				}
				require.Equal(t, "v0.1.1", version)
				require.False(t, done)
				require.Equal(t, 1, server.starts)
			})
		}
	}
}

func TestConvergeAgentMatchingVersionRequiresNoActiveInstallation(t *testing.T) {
	for _, test := range []struct {
		name        string
		operation   *pb.NodeAgentUpgradeOperation
		statusError error
		wantDone    bool
		wantStarts  int
		wantCode    codes.Code
	}{
		{name: "no upgrade record", wantDone: true},
		{name: "installation finished", operation: &pb.NodeAgentUpgradeOperation{Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_SUCCEEDED}, wantDone: true},
		{name: "installation still running", operation: &pb.NodeAgentUpgradeOperation{TargetVersion: "v0.1.2", Method: pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT, Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING}, wantStarts: 1},
		{name: "status unavailable", statusError: status.Error(codes.Unavailable, "status unreachable"), wantCode: codes.Unavailable},
		{name: "invalid request is not an unsupported RPC", statusError: status.Error(codes.InvalidArgument, "invalid request"), wantCode: codes.InvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &upgradeRPCServer{version: "v0.1.2", typed: true, operation: test.operation, statusError: test.statusError}
			client, node := upgradeTestClient(t, server)
			_, done, err := client.ConvergeNodeAgent(t.Context(), node, "v0.1.2", "apt")
			if test.statusError != nil {
				require.ErrorContains(t, err, "reading node-agent upgrade status:")
				require.Equal(t, test.wantCode, status.Code(err))
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.wantDone, done)
			require.Equal(t, test.wantStarts, server.starts)
		})
	}
}
