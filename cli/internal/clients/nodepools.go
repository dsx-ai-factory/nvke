// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"bytes"
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	api "github.com/NVIDIA/nke/controller/api"
)

// ListNodePools returns the node pools matching params. A nil params lists
// every node pool on the site controller.
func (c *CWEClient) ListNodePools(ctx context.Context, params *api.ListNodePoolsParams) ([]api.NodePool, error) {
	resp, err := c.raw.ListNodePoolsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("listing node pools: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing node pools: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing node pools: empty response body")
	}
	return resp.JSON200.NodePools, nil
}

// PatchNodePool applies a JSON merge patch to the node pool with the given
// UUID and returns the updated node pool. Only the fields the body carries are
// changed; the rest keep their stored values. The work the update implies,
// such as provisioning or tearing down nodes after a desired-count change,
// happens asynchronously.
func (c *CWEClient) PatchNodePool(
	ctx context.Context,
	id openapi_types.UUID,
	body []byte,
) (*api.NodePool, error) {
	resp, err := c.raw.UpdateNodePoolWithBodyWithResponse(ctx, id, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("patching node pool: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("patching node pool: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("patching node pool: empty response body")
	}
	return resp.JSON200, nil
}

// FindNodePoolByName resolves a node pool name within a cluster to its full
// NodePool record. Returns an error if the cluster holds no node pool with
// that name, or if several of them do, which would make name-based targeting
// ambiguous.
func (c *CWEClient) FindNodePoolByName(ctx context.Context, clusterName, name string) (*api.NodePool, error) {
	pools, err := c.ListNodePools(ctx, &api.ListNodePoolsParams{
		ClusterName: &clusterName,
		Name:        &name,
	})
	if err != nil {
		return nil, fmt.Errorf("finding node pool: %w", err)
	}
	switch len(pools) {
	case 0:
		return nil, fmt.Errorf("node pool %q not found in cluster %q", name, clusterName)
	case 1:
		return &pools[0], nil
	default:
		return nil, fmt.Errorf(
			"node pool name %q is ambiguous in cluster %q: %d node pools match", name, clusterName, len(pools))
	}
}
