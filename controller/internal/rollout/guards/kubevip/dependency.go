// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubevip

import (
	"context"

	"google.golang.org/grpc"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

type nodeAgentKubeVipClient interface {
	StartK8SControlPlaneNodeGracefulShutdown(ctx context.Context, req *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest, opts ...grpc.CallOption) (*nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error)
	GetK8SControlPlaneNodeGracefulShutdownStatus(ctx context.Context, req *nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest, opts ...grpc.CallOption) (*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error)
	StartKubeVipRestore(ctx context.Context, req *nkeproto.StartKubeVipRestoreRequest, opts ...grpc.CallOption) (*nkeproto.StartKubeVipRestoreResponse, error)
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
