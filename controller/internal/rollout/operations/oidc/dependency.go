// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package oidc

import (
	"context"

	"google.golang.org/grpc"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

// nodeAgentOIDCClient is the narrow slice of the node-agent gRPC surface the
// enablement operation uses. The generated nkeproto.NodeAgentClient satisfies
// it; tests inject a fake. Both enable and disable ride the one convergent
// PatchAPIServerManifest RPC (disable is the inverse patch).
type nodeAgentOIDCClient interface {
	PatchAPIServerManifest(ctx context.Context, req *nkeproto.PatchAPIServerManifestRequest, opts ...grpc.CallOption) (*nkeproto.PatchAPIServerManifestResponse, error)
}

// nodeAgentClientDialer opens a connection to a node's node-agent and returns a
// client plus a cleanup func.
type nodeAgentClientDialer func(ctx context.Context, node rollout.Node) (nodeAgentOIDCClient, func(), error)
