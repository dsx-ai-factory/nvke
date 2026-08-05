// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubevip

import (
	"context"

	"google.golang.org/grpc"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

type nodeAgentKubeVipClient interface {
	StartK8SControlPlaneNodeGracefulShutdown(ctx context.Context, req *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest, opts ...grpc.CallOption) (*nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error)
	GetK8SControlPlaneNodeGracefulShutdownStatus(ctx context.Context, req *nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest, opts ...grpc.CallOption) (*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error)
	StartKubeVipRestore(ctx context.Context, req *nkeproto.StartKubeVipRestoreRequest, opts ...grpc.CallOption) (*nkeproto.StartKubeVipRestoreResponse, error)
	GetNodeInfo(ctx context.Context, req *nkeproto.NodeInfoRequest, opts ...grpc.CallOption) (*nkeproto.NodeInfoResponse, error)
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
