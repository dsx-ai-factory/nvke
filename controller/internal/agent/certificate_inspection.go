// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// InspectKubernetesCertificates invokes only the bounded read-only inspection RPC.
// Wrapped errors retain Unimplemented so older agents are recorded as unsupported.
func (c *NodeAgentClient) InspectKubernetesCertificates(ctx context.Context, node db.Node) (*nvkeproto.InspectKubernetesCertificatesResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("connect for certificate inspection: %w", err)
	}
	defer cleanup()
	response, err := nvkeproto.NewNodeAgentClient(conn).InspectKubernetesCertificates(ctx, &nvkeproto.InspectKubernetesCertificatesRequest{ControlPlane: node.ControlPlane})
	if err != nil {
		return nil, fmt.Errorf("inspect node certificates: %w", err)
	}
	return response, nil
}
