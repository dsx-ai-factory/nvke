// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary

package client

import (
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	api "github.com/NVIDIA/nke/controller/api"
)

// ListClusters returns all clusters on the site controller.
func (c *CWEClient) ListClusters(ctx context.Context) ([]api.Cluster, error) {
	resp, err := c.raw.ListClustersWithResponse(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing clusters: unexpected status %s: %s", resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing clusters: empty response body")
	}
	return resp.JSON200.Clusters, nil
}

// CreateCluster submits a cluster creation request and returns the created cluster.
// The cluster will be in a provisioning state immediately after creation.
func (c *CWEClient) CreateCluster(ctx context.Context, req api.CreateClusterJSONRequestBody) (*api.Cluster, error) {
	resp, err := c.raw.CreateClusterWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("creating cluster: %w", err)
	}
	if resp.StatusCode() != 201 {
		return nil, fmt.Errorf("creating cluster: unexpected status %s: %s", resp.Status(), resp.Body)
	}
	if resp.JSON201 == nil {
		return nil, fmt.Errorf("creating cluster: empty response body")
	}
	return resp.JSON201, nil
}

// GetCluster returns a single cluster by UUID.
func (c *CWEClient) GetCluster(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error) {
	resp, err := c.raw.GetClusterWithResponse(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting cluster: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("getting cluster: unexpected status %s: %s", resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("getting cluster: empty response body")
	}
	return resp.JSON200, nil
}

// DeleteCluster deletes a cluster by UUID. Deletion is async.
func (c *CWEClient) DeleteCluster(ctx context.Context, id openapi_types.UUID) error {
	resp, err := c.raw.DeleteClusterWithResponse(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting cluster: %w", err)
	}
	if resp.StatusCode() != 202 {
		return fmt.Errorf("deleting cluster: unexpected status %s: %s", resp.Status(), resp.Body)
	}
	return nil
}

// FindClusterByName resolves a cluster name to its full Cluster record.
// Returns an error if no cluster with that name exists or if multiple clusters
// share the same name (which would make name-based targeting ambiguous).
// FIXME: NKX-14307 — verify whether the CWE API enforces cluster name uniqueness
// per site. If it does, the duplicate check here becomes unreachable and can be
// removed. If not, consider adding a GET /clusters?name= filter to avoid listing all.
func (c *CWEClient) FindClusterByName(ctx context.Context, name string) (*api.Cluster, error) {
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	var matches []*api.Cluster
	for i := range clusters {
		if clusters[i].Name == name {
			matches = append(matches, &clusters[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("cluster %q not found", name)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("cluster name %q is ambiguous: %d clusters match; use the cluster ID instead", name, len(matches))
	}
}
