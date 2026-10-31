// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type reconfigureArgsServer struct {
	pb.UnimplementedNodeAgentServer
	gotArgs  []string
	response *pb.ReconfigureNodeAgentArgsResponse
}

func (s *reconfigureArgsServer) ReconfigureNodeAgentArgs(
	_ context.Context,
	req *pb.ReconfigureNodeAgentArgsRequest,
) (*pb.ReconfigureNodeAgentArgsResponse, error) {
	s.gotArgs = req.GetArgs()
	return s.response, nil
}

func serveNodeAgent(t *testing.T, impl pb.NodeAgentServer) *NodeAgentClient {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	pb.RegisterNodeAgentServer(server, impl)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return NewClientWithTransport(&Transport{cfg: config.Config{NodeAgent: config.NodeAgentConfig{Port: port}}})
}

func TestReconfigureNodeAgentArgsPassesArgumentsAndOutcome(t *testing.T) {
	impl := &reconfigureArgsServer{response: &pb.ReconfigureNodeAgentArgsResponse{
		Outcome:        pb.ReconfigureNodeAgentArgsResponse_OUTCOME_APPLIED,
		RestartPending: true,
		RunningArgs:    []string{"--secure-server-port", "15005"},
	}}
	client := serveNodeAgent(t, impl)

	want := []string{"--secure-server-port", "15005", "--controller-audience=nvke-controller"}
	resp, err := client.ReconfigureNodeAgentArgs(t.Context(), db.Node{Name: "worker-1", IP: "127.0.0.1"}, want)
	require.NoError(t, err)
	assert.Equal(t, want, impl.gotArgs)
	assert.Equal(t, pb.ReconfigureNodeAgentArgsResponse_OUTCOME_APPLIED, resp.GetOutcome())
	assert.True(t, resp.GetRestartPending())
}

// An agent that predates the RPC has to fail loudly. Reading an Unimplemented
// as "nothing to do" would report a node as converged while its environment
// file still holds the old arguments.
func TestReconfigureNodeAgentArgsReportsOlderAgentAsUnsupported(t *testing.T) {
	client := serveNodeAgent(t, unimplementedAgentServer{})

	_, err := client.ReconfigureNodeAgentArgs(t.Context(), db.Node{Name: "worker-1", IP: "127.0.0.1"}, []string{"--secure-server-port", "15005"})
	require.ErrorContains(t, err, "upgrade the node agent")
}
