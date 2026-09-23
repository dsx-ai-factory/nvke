//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

type NodeAgentCall struct {
	ScriptName string
}

// NodeAgentServer is a test fixture reached through CWE's production agent client.
type NodeAgentServer struct {
	nkeproto.UnimplementedNodeAgentServer
	address    string
	calls      chan NodeAgentCall
	kubeconfig string
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
			return NodeAgentCall{}, fmt.Errorf("wait for agent script %q: %w", scriptName, ctx.Err())
		}
	}
}

// SetKubeconfig configures the tenant kubeconfig returned by GetKubeconfig.
func (s *NodeAgentServer) SetKubeconfig(kubeconfig string) {
	s.kubeconfig = kubeconfig
}

func (s *NodeAgentServer) GetKubeconfig(
	_ context.Context,
	_ *nkeproto.KubeconfigRequest,
) (*nkeproto.KubeconfigResponse, error) {
	return &nkeproto.KubeconfigResponse{Kubeconfig: s.kubeconfig}, nil
}

func (s *NodeAgentServer) ExecuteScript(_ context.Context, req *nkeproto.ExecuteScriptRequest) (*nkeproto.ExecuteScriptResponse, error) {
	s.calls <- NodeAgentCall{ScriptName: req.GetScriptName()}
	return &nkeproto.ExecuteScriptResponse{
		Output:   "gofail e2e agent response",
		Status:   "succeeded",
		ExitCode: 0,
	}, nil
}
