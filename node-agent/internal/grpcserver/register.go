// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package grpcserver

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
)

// Register installs the NodeAgent API and gRPC server reflection (v1 and v1alpha) on s.
// Reflection lets clients such as grpcurl discover services without a local copy of
// node_agent.proto, and lets orchestrators probe for RPC availability instead of
// comparing agent semver strings.
func Register(s *grpc.Server, agent pb.NodeAgentServer) {
	pb.RegisterNodeAgentServer(s, agent)
	reflection.Register(s)
}
