// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"

	"google.golang.org/grpc"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

// nodeAgentOIDCClient is the narrow slice of the agent gRPC surface the
// enablement operation uses. The generated nkeproto.NodeAgentClient satisfies
// it; tests inject a fake. Both enable and disable ride the one convergent
// PatchAPIServerManifest RPC (disable is the inverse patch).
type nodeAgentOIDCClient interface {
	PatchAPIServerManifest(ctx context.Context, req *nkeproto.PatchAPIServerManifestRequest, opts ...grpc.CallOption) (*nkeproto.PatchAPIServerManifestResponse, error)
}

// nodeAgentClientDialer opens a connection to a node's agent and returns a
// client plus a cleanup func.
type nodeAgentClientDialer func(ctx context.Context, node rollout.Node) (nodeAgentOIDCClient, func(), error)
