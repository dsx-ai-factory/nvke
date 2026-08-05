//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package framework

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

type NodeAgentCall struct {
	ScriptName string
}

// NodeAgentServer is a test fixture reached through CWE's production node-agent client.
type NodeAgentServer struct {
	nkeproto.UnimplementedNodeAgentServer
	address string
	calls   chan NodeAgentCall
}

func StartNodeAgentServer(t testing.TB) *NodeAgentServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	fixture := &NodeAgentServer{
		address: listener.Addr().String(),
		calls:   make(chan NodeAgentCall, 32),
	}
	server := grpc.NewServer()
	nkeproto.RegisterNodeAgentServer(server, fixture)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)
	return fixture
}

func (s *NodeAgentServer) Port(t testing.TB) string {
	t.Helper()
	_, port, err := net.SplitHostPort(s.address)
	require.NoError(t, err)
	return port
}

func (s *NodeAgentServer) WaitForScript(ctx context.Context, scriptName string) (NodeAgentCall, error) {
	for {
		select {
		case call := <-s.calls:
			if call.ScriptName == scriptName {
				return call, nil
			}
		case <-ctx.Done():
			return NodeAgentCall{}, fmt.Errorf("wait for node-agent script %q: %w", scriptName, ctx.Err())
		}
	}
}

func (s *NodeAgentServer) ExecuteScript(_ context.Context, req *nkeproto.ExecuteScriptRequest) (*nkeproto.ExecuteScriptResponse, error) {
	s.calls <- NodeAgentCall{ScriptName: req.GetScriptName()}
	return &nkeproto.ExecuteScriptResponse{
		Output:   "gofail e2e node-agent response",
		Status:   "succeeded",
		ExitCode: 0,
	}, nil
}
