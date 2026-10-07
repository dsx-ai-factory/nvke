// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	api "github.com/dsx-ai-factory/nke/controller/api"
)

// ListControlPlaneNodes returns the control-plane nodes of one cluster.
//
// Control-plane only, because the callers that need node-level detail are
// waiting on the control plane: a worker's state says nothing about whether
// the cluster can be left to run on its own.
func (c *ControllerClient) ListControlPlaneNodes(
	ctx context.Context,
	clusterID openapi_types.UUID,
) ([]api.Node, error) {
	controlPlane := true
	resp, err := c.raw.ListNodesWithResponse(ctx, &api.ListNodesParams{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return nil, fmt.Errorf("listing control-plane nodes: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing control-plane nodes: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing control-plane nodes: empty response body")
	}
	return resp.JSON200.Nodes, nil
}
