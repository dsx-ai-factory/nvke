// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package nke offers utilities to interact with the NKE management plane.
// Like a thin client over the CWE generated API, reached through the
// management apiserver's Service proxy (no localhost / port-forward).
package nke

import (
	"context"
	"fmt"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/nke/controller/api"
)

// Default waiter timeouts when Options fields are zero.
const (
	defaultProvision = 75 * time.Minute
	defaultDelete    = 40 * time.Minute
	defaultPoll      = 30 * time.Second
)

// Options tunes the waiter loops. Zero values fall back to the defaults
// (75m provision / 40m delete / 30s poll).
type Options struct {
	Provision, Upgrade, Delete, Poll time.Duration
}

// Client wraps the generated CWE API client plus the configured timeouts.
type Client struct {
	api  *api.ClientWithResponses
	opts Options
}

// NewClient builds the generated API client over the kube apiserver service-proxy.
// The proxy forwards /proxy/api/clusters to the CWE Service's /api/clusters,
// reusing the REST config's auth and TLS transport.
func NewClient(restConfig *rest.Config, opts Options) (*Client, error) {
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building HTTP client: %w", err)
	}
	base := strings.TrimRight(restConfig.Host, "/") +
		"/api/v1/namespaces/cloud-workflow-engine/services/cloud-workflow-engine:8080/proxy/api"
	apiClient, err := api.NewClientWithResponses(base, api.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("building API client: %w", err)
	}
	return &Client{api: apiClient, opts: withDefaults(opts)}, nil
}

// API exposes the underlying generated client for preflight checks.
func (c *Client) API() *api.ClientWithResponses { return c.api }

// ListNodePools returns the node pools owned by cluster id.
func (c *Client) ListNodePools(ctx context.Context, clusterID openapi_types.UUID) ([]api.NodePool, error) {
	resp, err := c.api.ListNodePoolsWithResponse(ctx, &api.ListNodePoolsParams{ClusterID: &clusterID})
	if err != nil {
		return nil, fmt.Errorf("listing node pools for cluster %s: %w", clusterID, err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing node pools for cluster %s: unexpected status %s: %s", clusterID, resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing node pools for cluster %s: empty response body", clusterID)
	}
	return resp.JSON200.NodePools, nil
}

// ListClusterNodes returns the nodes owned by cluster id.
func (c *Client) ListClusterNodes(ctx context.Context, clusterID openapi_types.UUID) ([]api.Node, error) {
	resp, err := c.api.ListNodesWithResponse(ctx, &api.ListNodesParams{ClusterID: &clusterID})
	if err != nil {
		return nil, fmt.Errorf("listing nodes for cluster %s: %w", clusterID, err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing nodes for cluster %s: unexpected status %s: %s", clusterID, resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing nodes for cluster %s: empty response body", clusterID)
	}
	return resp.JSON200.Nodes, nil
}

// ListNodePoolNodes returns the nodes owned by node pool id.
func (c *Client) ListNodePoolNodes(ctx context.Context, nodePoolID openapi_types.UUID) ([]api.Node, error) {
	resp, err := c.api.ListNodesWithResponse(ctx, &api.ListNodesParams{NodePoolID: &nodePoolID})
	if err != nil {
		return nil, fmt.Errorf("listing nodes for node pool %s: %w", nodePoolID, err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing nodes for node pool %s: unexpected status %s: %s", nodePoolID, resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing nodes for node pool %s: empty response body", nodePoolID)
	}
	return resp.JSON200.Nodes, nil
}

// GetNode returns the node with the given id.
func (c *Client) GetNode(ctx context.Context, nodeID openapi_types.UUID) (*api.Node, error) {
	resp, err := c.api.GetNodeWithResponse(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("getting node %s: %w", nodeID, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("getting node %s: unexpected status %s: %s", nodeID, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// ProvisionedNodeCount returns how many nodes in the pool have a True
// Provisioned condition.
func (c *Client) ProvisionedNodeCount(ctx context.Context, nodePoolID openapi_types.UUID) (int, error) {
	nodes, err := c.ListNodePoolNodes(ctx, nodePoolID)
	if err != nil {
		return 0, err
	}
	provisioned := 0
	for _, node := range nodes {
		if node.Conditions.Provisioned.Status == api.ConditionStatusTrue {
			provisioned++
		}
	}
	return provisioned, nil
}

// CreateCluster builds a create request from spec and posts it. It requires a
// 201 response and returns the created cluster.
func (c *Client) CreateCluster(ctx context.Context, spec ClusterSpec) (*api.Cluster, error) {
	req, err := buildCreateRequest(spec)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.CreateClusterWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("creating cluster: %w", err)
	}
	if resp.StatusCode() == 400 && resp.JSON400 != nil {
		return nil, fmt.Errorf("creating cluster %q: %s", req.Name, resp.JSON400.Message)
	}
	if resp.StatusCode() != 201 || resp.JSON201 == nil {
		return nil, fmt.Errorf("creating cluster %q: unexpected status %s: %s", req.Name, resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

// DeleteCluster requests deletion of the cluster and requires a 202 response.
func (c *Client) DeleteCluster(ctx context.Context, id openapi_types.UUID) error {
	resp, err := c.api.DeleteClusterWithResponse(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting cluster: %w", err)
	}
	if resp.StatusCode() != 202 {
		return fmt.Errorf("deleting cluster %s: unexpected status %s: %s", id, resp.Status(), resp.Body)
	}
	return nil
}

func (c *Client) UpdateNodePoolDesiredCount(ctx context.Context, id openapi_types.UUID, desired int) (*api.NodePool, error) {
	resp, err := c.api.UpdateNodePoolWithResponse(ctx, id, api.UpdateNodePoolRequest{
		DesiredNodeCount: &desired,
	})
	if err != nil {
		return nil, fmt.Errorf("updating node pool %s desired count: %w", id, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("updating node pool %s desired count to %d: unexpected status %s: %s", id, desired, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

func (c *Client) DeleteNode(ctx context.Context, id openapi_types.UUID) error {
	resp, err := c.api.DeleteNodeWithResponse(ctx, id, api.DeleteNodeRequest{})
	if err != nil {
		return fmt.Errorf("deleting node %s: %w", id, err)
	}
	if resp.StatusCode() != 202 {
		return fmt.Errorf("deleting node %s: unexpected status %s: %s", id, resp.Status(), resp.Body)
	}
	return nil
}

// CreateNodePool adds a node pool to an existing cluster and requires a 201
// response, returning the created node pool.
func (c *Client) CreateNodePool(ctx context.Context, req api.CreateNodePoolRequest) (*api.NodePool, error) {
	resp, err := c.api.CreateNodePoolWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("creating node pool: %w", err)
	}
	if resp.StatusCode() == 400 && resp.JSON400 != nil {
		return nil, fmt.Errorf("creating node pool %q: %s", req.Name, resp.JSON400.Message)
	}
	if resp.StatusCode() != 201 || resp.JSON201 == nil {
		return nil, fmt.Errorf("creating node pool %q: unexpected status %s: %s", req.Name, resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

// DeleteNodePool requests deletion of the node pool and requires a 202 response.
func (c *Client) DeleteNodePool(ctx context.Context, id openapi_types.UUID) error {
	resp, err := c.api.DeleteNodePoolWithResponse(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting node pool %s: %w", id, err)
	}
	if resp.StatusCode() != 202 {
		return fmt.Errorf("deleting node pool %s: unexpected status %s: %s", id, resp.Status(), resp.Body)
	}
	return nil
}

// GetNodePool returns the node pool and the HTTP status code. A 404 yields
// (nil, 404, nil) so callers can distinguish "gone" from an error.
func (c *Client) GetNodePool(ctx context.Context, id openapi_types.UUID) (*api.NodePool, int, error) {
	resp, err := c.api.GetNodePoolWithResponse(ctx, id)
	if err != nil {
		return nil, 0, fmt.Errorf("getting node pool %s: %w", id, err)
	}
	if resp.StatusCode() == 404 {
		return nil, 404, nil
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, resp.StatusCode(), fmt.Errorf("getting node pool %s: unexpected status %s: %s", id, resp.Status(), resp.Body)
	}
	return resp.JSON200, 200, nil
}

// GetByName returns the cluster with the given name, or an error if the list
// call fails or no cluster matches.
func (c *Client) GetByName(ctx context.Context, name string) (*api.Cluster, error) {
	resp, err := c.api.ListClustersWithResponse(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing clusters: unexpected status %s", resp.Status())
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing clusters: empty response body")
	}
	for i := range resp.JSON200.Clusters {
		if resp.JSON200.Clusters[i].Name == name {
			return &resp.JSON200.Clusters[i], nil
		}
	}
	return nil, fmt.Errorf("cluster %q not found", name)
}

func withDefaults(opts Options) Options {
	if opts.Provision <= 0 {
		opts.Provision = defaultProvision
	}
	if opts.Upgrade <= 0 {
		opts.Upgrade = defaultProvision
	}
	if opts.Delete <= 0 {
		opts.Delete = defaultDelete
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultPoll
	}
	return opts
}
