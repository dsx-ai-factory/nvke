// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubevip

import (
	"context"

	"google.golang.org/grpc"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
)

type nodeAgentKubeVipClient interface {
	StartK8SControlPlaneNodeGracefulShutdown(ctx context.Context, req *nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest, opts ...grpc.CallOption) (*nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error)
	GetK8SControlPlaneNodeGracefulShutdownStatus(ctx context.Context, req *nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest, opts ...grpc.CallOption) (*nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error)
	StartKubeVipRestore(ctx context.Context, req *nvkeproto.StartKubeVipRestoreRequest, opts ...grpc.CallOption) (*nvkeproto.StartKubeVipRestoreResponse, error)
}

type nodeAgentKubeVipDialer func(ctx context.Context, node rollout.Node) (nodeAgentKubeVipClient, func(), error)

func adaptDialer(dialer nodeagent.Dialer) nodeAgentKubeVipDialer {
	if dialer == nil {
		return nil
	}
	return func(ctx context.Context, node rollout.Node) (nodeAgentKubeVipClient, func(), error) {
		return dialer(ctx, node)
	}
}
