//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

type NodeAgentCall struct {
	ScriptName string
}

// NodeAgentServer is a test fixture reached through the controller's production agent client.
type NodeAgentServer struct {
	nvkeproto.UnimplementedNodeAgentServer
	address       string
	calls         chan NodeAgentCall
	kubeconfig    string
	rebootMu      sync.Mutex
	rebootEnabled bool
	rebootErr     error
	rebootCalls   int
	reboots       chan time.Time
}

func StartNodeAgentServer(t testing.TB) *NodeAgentServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	fixture := &NodeAgentServer{
		address: listener.Addr().String(),
		calls:   make(chan NodeAgentCall, 32),
		reboots: make(chan time.Time, 32),
	}
	server := grpc.NewServer()
	nvkeproto.RegisterNodeAgentServer(server, fixture)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)
	return fixture
}

// SetRebootError enables typed reboot requests without changing boot state.
// A nil error models an accepted request whose reboot never completes.
func (s *NodeAgentServer) SetRebootError(err error) {
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	s.rebootEnabled = true
	s.rebootErr = err
}

// RebootCalls returns the number of typed reboot requests received.
func (s *NodeAgentServer) RebootCalls() int {
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	return s.rebootCalls
}

// WaitForReboot waits for a typed reboot request and returns its receipt time.
func (s *NodeAgentServer) WaitForReboot(ctx context.Context) (time.Time, error) {
	select {
	case at := <-s.reboots:
		return at, nil
	case <-ctx.Done():
		return time.Time{}, fmt.Errorf("wait for reboot request: %w", ctx.Err())
	}
}

// ScheduleNodeReboot simulates scheduling independently of host boot progress.
func (s *NodeAgentServer) ScheduleNodeReboot(_ context.Context, _ *nvkeproto.ScheduleNodeRebootRequest) (*nvkeproto.ScheduleNodeRebootResponse, error) {
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	if !s.rebootEnabled {
		return nil, status.Error(codes.Unimplemented, "typed reboot fixture is disabled")
	}
	s.rebootCalls++
	s.reboots <- time.Now()
	if s.rebootErr != nil {
		return nil, s.rebootErr
	}
	return &nvkeproto.ScheduleNodeRebootResponse{}, nil
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
	_ *nvkeproto.KubeconfigRequest,
) (*nvkeproto.KubeconfigResponse, error) {
	return &nvkeproto.KubeconfigResponse{Kubeconfig: s.kubeconfig}, nil
}

func (s *NodeAgentServer) ExecuteScript(_ context.Context, req *nvkeproto.ExecuteScriptRequest) (*nvkeproto.ExecuteScriptResponse, error) {
	s.calls <- NodeAgentCall{ScriptName: req.GetScriptName()}
	return &nvkeproto.ExecuteScriptResponse{
		Output:   "gofail e2e agent response",
		Status:   "succeeded",
		ExitCode: 0,
	}, nil
}
