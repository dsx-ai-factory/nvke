// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/descriptorpb"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestBootstrapRejectsOlderAgentsForRotatedStorageIdentity(t *testing.T) {
	legacy := currentNodeAgentDescriptor()
	for _, message := range legacy.GetMessageType() {
		fields := make([]*descriptorpb.FieldDescriptorProto, 0, len(message.GetField()))
		for _, field := range message.GetField() {
			if field.GetName() != "api_encryption_key_name" {
				fields = append(fields, field)
			}
		}
		message.Field = fields
	}
	for _, test := range []struct {
		name         string
		descriptor   *descriptorpb.FileDescriptorProto
		noReflection bool
		keyName      string
		wantError    bool
	}{
		{name: "new agent retains rotated name", descriptor: currentNodeAgentDescriptor(), keyName: "kube-api-encryption-key-7"},
		{name: "old descriptor cannot silently ignore name", descriptor: legacy, keyName: "kube-api-encryption-key-7", wantError: true},
		{name: "missing reflection rejects rotated name", noReflection: true, keyName: "kube-api-encryption-key-7", wantError: true},
		{name: "legacy bootstrap remains compatible", noReflection: true, keyName: constants.LegacyAPIEncryptionKeyName},
		{name: "old request remains compatible", noReflection: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := newReflectionTestConnection(t, test.descriptor, test.noReflection)
			err := requireKubeadmEncryptionKeyName(context.Background(), conn, test.keyName)
			if test.wantError {
				require.ErrorContains(t, err, "upgrade")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

type unimplementedAgentServer struct {
	pb.UnimplementedNodeAgentServer
}

func TestGetEtcdEncryptionKeyIdentityReportsOlderAgentAsUnsupported(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	pb.RegisterNodeAgentServer(server, unimplementedAgentServer{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	client := NewClientWithTransport(&Transport{cfg: config.Config{NodeAgent: config.NodeAgentConfig{Port: port}}})

	_, err = client.GetEtcdEncryptionKeyIdentity(t.Context(), db.Node{Name: "cp-0", IP: "127.0.0.1"}, "digest")
	require.ErrorIs(t, err, ErrEncryptionIdentityUnsupported)
}
