// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ErrEncryptionIdentityUnsupported reports a node agent that predates the
// GetEtcdEncryptionKeyIdentity RPC.
var ErrEncryptionIdentityUnsupported = errors.New("requires an agent upgrade for encryption identity discovery")

// GetEtcdEncryptionKeyIdentity reads the storage name corresponding to a key
// digest. Errors deliberately omit the request and upstream diagnostic text.
func (c *NodeAgentClient) GetEtcdEncryptionKeyIdentity(ctx context.Context, node db.Node, digest string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return "", fmt.Errorf("cannot connect to node %s for encryption identity discovery", node.Name)
	}
	defer cleanup()
	resp, err := nvkeproto.NewNodeAgentClient(conn).GetEtcdEncryptionKeyIdentity(ctx, &nvkeproto.GetEtcdEncryptionKeyIdentityRequest{KeySha256: digest})
	if status.Code(err) == codes.Unimplemented {
		return "", fmt.Errorf("node %s %w", node.Name, ErrEncryptionIdentityUnsupported)
	}
	if err != nil {
		return "", fmt.Errorf("cannot discover encryption storage identity on node %s", node.Name)
	}
	return resp.GetKeyName(), nil
}

// Old agents ignore unknown proto fields. Only the original bootstrap name is
// safe without capability confirmation; a rotated identity must fail closed.
func requireKubeadmEncryptionKeyName(ctx context.Context, conn grpc.ClientConnInterface, name string) error {
	if name == "" || name == constants.LegacyAPIEncryptionKeyName {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return errors.New("cannot inspect encryption key name capability; upgrade the node agent before bootstrap")
	}
	defer func() { _ = stream.CloseSend() }()
	for _, symbol := range []string{"proto.KubeadmInitRequest.api_encryption_key_name", "proto.KubeadmJoinRequest.api_encryption_key_name"} {
		if err := stream.Send(&reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol}}); err != nil && !errors.Is(err, io.EOF) {
			return errors.New("cannot inspect encryption key name capability; upgrade the node agent before bootstrap")
		}
		resp, err := stream.Recv()
		if err != nil || resp.GetErrorResponse() != nil || resp.GetFileDescriptorResponse() == nil {
			return errors.New("node agent cannot preserve the encryption storage identity; upgrade it before bootstrap")
		}
	}
	return nil
}
