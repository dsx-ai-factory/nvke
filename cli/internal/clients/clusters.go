// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"bytes"
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	api "github.com/dsx-ai-factory/nke/controller/api"
)

// ListClusters returns the clusters matching params. A nil params lists every
// cluster on the site controller.
func (c *CWEClient) ListClusters(ctx context.Context, params *api.ListClustersParams) ([]api.Cluster, error) {
	resp, err := c.raw.ListClustersWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing clusters: %w", apiError(resp.Status(), resp.Body))
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
		return nil, fmt.Errorf("creating cluster: %w", apiError(resp.Status(), resp.Body))
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
		return nil, fmt.Errorf("getting cluster: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("getting cluster: empty response body")
	}
	return resp.JSON200, nil
}

// PatchCluster PATCHes the cluster identified by id with body, a JSON
// document matching UpdateClusterRequest, and returns the cluster as it now
// stands.
func (c *CWEClient) PatchCluster(ctx context.Context, id openapi_types.UUID, body []byte) (*api.Cluster, error) {
	resp, err := c.raw.UpdateClusterWithBodyWithResponse(ctx, id, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("patching cluster: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("patching cluster: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("patching cluster: empty response body")
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
// The duplicate check is unreachable in practice: the DB enforces a global unique
// index on cluster name (migration 20250407125309_name_uniqueness.go, index
// clusters_name_unique), so the API rejects duplicate names at creation time.
func (c *CWEClient) FindClusterByName(ctx context.Context, name string) (*api.Cluster, error) {
	clusters, err := c.ListClusters(ctx, &api.ListClustersParams{Name: &name})
	if err != nil {
		return nil, fmt.Errorf("finding cluster: %w", err)
	}
	switch len(clusters) {
	case 0:
		return nil, fmt.Errorf("cluster %q not found", name)
	case 1:
		return &clusters[0], nil
	default:
		return nil, fmt.Errorf("cluster name %q is ambiguous: %d clusters match", name, len(clusters))
	}
}
