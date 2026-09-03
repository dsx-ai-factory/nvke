// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

func TestSupportsKubeadmFilesViaReflection(t *testing.T) {
	tests := map[string]struct {
		descriptor         *descriptorpb.FileDescriptorProto
		reflectionDisabled bool
		wantSupported      bool
	}{
		"current agent exposes both fields": {
			descriptor:    currentNodeAgentDescriptor(),
			wantSupported: true,
		},
		"legacy agent exposes neither field": {
			descriptor: descriptorWithoutKubeadmFiles(),
		},
		"agent missing one field uses fallback": {
			descriptor: descriptorWithoutKubeadmFiles("KubeadmJoinRequest"),
		},
		"agent without reflection uses fallback": {
			reflectionDisabled: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			conn := newReflectionTestConnection(t, test.descriptor, test.reflectionDisabled)

			supported, err := supportsKubeadmFiles(context.Background(), conn)

			require.NoError(t, err)
			require.Equal(t, test.wantSupported, supported)
		})
	}
}

func TestSupportsKubeadmFilesRejectsNilConnection(t *testing.T) {
	_, err := supportsKubeadmFiles(context.Background(), nil)

	require.EqualError(t, err, "grpc connection is nil")
}

func currentNodeAgentDescriptor() *descriptorpb.FileDescriptorProto {
	return protodesc.ToFileDescriptorProto(
		(&nkeproto.KubeadmInitRequest{}).ProtoReflect().Descriptor().ParentFile(),
	)
}

func descriptorWithoutKubeadmFiles(messages ...string) *descriptorpb.FileDescriptorProto {
	if len(messages) == 0 {
		messages = []string{"KubeadmInitRequest", "KubeadmJoinRequest"}
	}
	removeFrom := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		removeFrom[message] = struct{}{}
	}

	descriptor := currentNodeAgentDescriptor()
	for _, message := range descriptor.GetMessageType() {
		if _, ok := removeFrom[message.GetName()]; !ok {
			continue
		}
		fields := message.GetField()[:0]
		for _, field := range message.GetField() {
			if field.GetName() != "files" {
				fields = append(fields, field)
			}
		}
		message.Field = fields
	}
	return descriptor
}

func newReflectionTestConnection(
	t *testing.T,
	descriptor *descriptorpb.FileDescriptorProto,
	reflectionDisabled bool,
) *grpc.ClientConn {
	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	if !reflectionDisabled {
		file, err := protodesc.NewFile(descriptor, protoregistry.GlobalFiles)
		require.NoError(t, err)
		files := &protoregistry.Files{}
		require.NoError(t, files.RegisterFile(file))
		reflectionpb.RegisterServerReflectionServer(server, reflection.NewServerV1(reflection.ServerOptions{
			DescriptorResolver: files,
		}))
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///node-agent",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
