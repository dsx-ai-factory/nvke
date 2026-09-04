// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// SupportsKubeadmFiles reports whether the node agent accepts staged files on
// both kubeadm init and kubeadm join requests.
func (c *NodeAgentClient) SupportsKubeadmFiles(ctx context.Context, node db.Node) (bool, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return false, fmt.Errorf("failed to connect to node agent for kubeadm file capability on node %s: %w", node.Name, err)
	}
	defer cleanup()

	supported, err := supportsKubeadmFiles(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("failed to inspect kubeadm file capability on node %s: %w", node.Name, err)
	}
	return supported, nil
}

func supportsKubeadmFiles(ctx context.Context, conn grpc.ClientConnInterface) (bool, error) {
	if conn == nil {
		return false, fmt.Errorf("grpc connection is nil")
	}

	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return false, fmt.Errorf("open grpc reflection stream: %w", err)
	}
	defer func() { _ = stream.CloseSend() }()

	symbols, err := kubeadmFilesFieldSymbols()
	if err != nil {
		return false, err
	}
	for _, symbol := range symbols {
		if err := stream.Send(&reflectionpb.ServerReflectionRequest{
			MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
				FileContainingSymbol: symbol,
			},
		}); err != nil && !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("query grpc reflection for %s: %w", symbol, err)
		}

		// A server-side stream failure is reported by Send as io.EOF; Recv
		// carries the actual gRPC status, including Unimplemented from agents
		// that do not expose reflection.
		resp, err := stream.Recv()
		if status.Code(err) == codes.Unimplemented {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("receive grpc reflection response for %s: %w", symbol, err)
		}
		if reflectionErr := resp.GetErrorResponse(); reflectionErr != nil {
			if reflectionErr.GetErrorCode() == int32(codes.NotFound) {
				return false, nil
			}
			return false, fmt.Errorf(
				"grpc reflection failed for %s: code=%d message=%s",
				symbol,
				reflectionErr.GetErrorCode(),
				reflectionErr.GetErrorMessage(),
			)
		}
		if resp.GetFileDescriptorResponse() == nil {
			return false, fmt.Errorf("grpc reflection returned no descriptor for %s", symbol)
		}
	}

	return true, nil
}

func kubeadmFilesFieldSymbols() ([]string, error) {
	messages := []protoreflect.MessageDescriptor{
		(&nkeproto.KubeadmInitRequest{}).ProtoReflect().Descriptor(),
		(&nkeproto.KubeadmJoinRequest{}).ProtoReflect().Descriptor(),
	}
	symbols := make([]string, 0, len(messages))
	for _, message := range messages {
		field := message.Fields().ByName("files")
		if field == nil {
			return nil, fmt.Errorf("local protobuf descriptor %s has no files field", message.FullName())
		}
		symbols = append(symbols, string(field.FullName()))
	}
	return symbols, nil
}
