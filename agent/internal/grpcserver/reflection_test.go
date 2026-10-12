// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package grpcserver_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dsx-ai-factory/nvke/agent/internal/grpcserver"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const bufConnSize = 1024 * 1024

func startReflectionTestServer(t *testing.T) (*grpc.ClientConn, func()) {
	t.Helper()

	lis := bufconn.Listen(bufConnSize)
	s := grpc.NewServer()
	grpcserver.Register(s, pb.UnimplementedNodeAgentServer{})

	go func() {
		_ = s.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	assert.NoError(t, err)

	return conn, func() {
		conn.Close()
		s.Stop()
	}
}

func TestReflectionListsNodeAgentService(t *testing.T) {
	conn, cleanup := startReflectionTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cli := reflectionpb.NewServerReflectionClient(conn)
	stream, err := cli.ServerReflectionInfo(ctx)
	assert.NoError(t, err)

	assert.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{},
	}))

	resp, err := stream.Recv()
	assert.NoError(t, err)

	list := resp.GetListServicesResponse()
	assert.NotNil(t, list, "expected ListServicesResponse, got %T", resp.MessageResponse)

	var foundNodeAgent bool
	for _, svc := range list.GetService() {
		if svc.GetName() == "proto.NodeAgent" {
			foundNodeAgent = true
			break
		}
	}
	assert.True(t, foundNodeAgent, "proto.NodeAgent not listed in reflection services")
}

func TestReflectionExposesInstallCNIMethod(t *testing.T) {
	conn, cleanup := startReflectionTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cli := reflectionpb.NewServerReflectionClient(conn)
	stream, err := cli.ServerReflectionInfo(ctx)
	assert.NoError(t, err)

	assert.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: "proto.NodeAgent.InstallCNI",
		},
	}))

	resp, err := stream.Recv()
	assert.NoError(t, err)
	assert.NotNil(t, resp.GetFileDescriptorResponse(),
		"expected FileDescriptorResponse for InstallCNI symbol, got %T", resp.MessageResponse)
}
