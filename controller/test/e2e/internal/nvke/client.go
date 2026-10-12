// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nke offers utilities to interact with the NVKE management plane.
// Like a thin client over the NVKE generated API, reached over a port-forward to
// the NVKE Service's backing pod. The client carries an NVKE credential of its
// own, separate from the kubeconfig credential authenticating the forward.
package nvke

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

// Default waiter timeouts when Options fields are zero.
const (
	defaultProvision = 75 * time.Minute
	defaultDelete    = 40 * time.Minute
	defaultPoll      = 30 * time.Second
)

const (
	// discoverTimeout bounds the cluster reads NewClient does to work out whether
	// NVKE is authenticating and for what audience.
	discoverTimeout = 30 * time.Second
)

// Options tunes the waiter loops. Zero values fall back to the defaults above.
type Options struct {
	Provision, Upgrade, Delete, Poll time.Duration
}

// TokenSource yields the bearer token to present to the NVKE API. Satisfied by
// *authtoken.Source; an interface so a spec can hand over a wrong identity.
type TokenSource interface {
	// Token must refresh: one client serves a whole provisioning wait,
	// outlasting any token lifetime.
	Token(ctx context.Context) (string, error)
}

// Client wraps the generated controller API client plus the configured timeouts.
type Client struct {
	api  *api.ClientWithResponses
	opts Options
}

// NewClient builds the generated API client over a port-forward to the NVKE
// Service's backing pod, shared process-wide and closed by
// kube.CloseSharedForwarders. It discovers its own credential, so every spec
// presents one. ctx bounds only the discovery reads.
func NewClient(ctx context.Context, restConfig *rest.Config, opts Options) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	source, found, err := authtoken.Discover(ctx, restConfig)
	if err != nil {
		return nil, fmt.Errorf("discovering the NVKE API credential: %w", err)
	}
	// A typed nil in an interface is not a nil interface, so the no-credential
	// case leaves tokens unassigned.
	var tokens TokenSource
	if found {
		tokens = source
	}
	return NewClientWithTokenSource(restConfig, opts, tokens)
}

// NewClientWithTokenSource is NewClient with the credential supplied rather
// than discovered. A nil tokens sends no Authorization header, which is both
// the no-authentication deployment and the 401 case.
func NewClientWithTokenSource(restConfig *rest.Config, opts Options, tokens TokenSource) (*Client, error) {
	forwarder, err := kube.SharedServiceForwarder(restConfig, controller.Namespace(), controller.Service(), controller.Port())
	if err != nil {
		return nil, fmt.Errorf("building NVKE port-forward: %w", err)
	}

	clientOpts := []api.ClientOption{api.WithHTTPClient(forwarder.HTTPClient())}
	if tokens != nil {
		clientOpts = append(clientOpts, api.WithRequestEditorFn(bearerToken(tokens)))
	}

	apiClient, err := api.NewClientWithResponses(forwarder.BaseURL()+"/api", clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("building API client: %w", err)
	}
	return &Client{api: apiClient, opts: WithDefaults(opts)}, nil
}

// bearerToken sets Authorization on every request. The token is fetched per
// request, so the source's refresh is what the request sees. A failure to mint
// is an error rather than a bare request that would read as a 401.
func bearerToken(tokens TokenSource) api.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		token, err := tokens.Token(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

// API exposes the underlying generated client for preflight checks.
func (c *Client) API() *api.ClientWithResponses { return c.api }

// ListClusters returns every cluster known to NVKE.
func (c *Client) ListClusters(ctx context.Context) ([]api.Cluster, error) {
	resp, err := c.api.ListClustersWithResponse(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing clusters: unexpected status %s: %s", resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, errors.New("listing clusters: empty response body")
	}
	return resp.JSON200.Clusters, nil
}

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

// ListClusterNodesByControlPlane returns the cluster's nodes filtered by
// control-plane membership: controlPlane=true returns only control-plane nodes,
// false only workers. It exercises the ListNodes controlPlane query filter.
func (c *Client) ListClusterNodesByControlPlane(ctx context.Context, clusterID openapi_types.UUID, controlPlane bool) ([]api.Node, error) {
	resp, err := c.api.ListNodesWithResponse(ctx, &api.ListNodesParams{ClusterID: &clusterID, ControlPlane: &controlPlane})
	if err != nil {
		return nil, fmt.Errorf("listing nodes for cluster %s (controlPlane=%t): %w", clusterID, controlPlane, err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("listing nodes for cluster %s (controlPlane=%t): unexpected status %s: %s", clusterID, controlPlane, resp.Status(), resp.Body)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing nodes for cluster %s (controlPlane=%t): empty response body", clusterID, controlPlane)
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

// CreateCluster builds and submits a cluster request.
func (c *Client) CreateCluster(ctx context.Context, spec ClusterSpec) (*api.Cluster, error) {
	req, err := BuildCreateRequest(spec)
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

// UpdateNodePoolDesiredCountExpectingRefusal requires a desired-count update
// to fail with 400 and returns the server's validation message.
func (c *Client) UpdateNodePoolDesiredCountExpectingRefusal(ctx context.Context, id openapi_types.UUID, desired int) (string, error) {
	resp, err := c.api.UpdateNodePoolWithResponse(ctx, id, api.UpdateNodePoolRequest{
		DesiredNodeCount: &desired,
	})
	if err != nil {
		return "", fmt.Errorf("updating node pool %s desired count: %w", id, err)
	}
	if resp.StatusCode() != 400 || resp.JSON400 == nil {
		return "", fmt.Errorf("updating node pool %s desired count to %d: want 400, got %s: %s", id, desired, resp.Status(), resp.Body)
	}
	return resp.JSON400.Message, nil
}

// UpdateNodePoolSystemReservedMemoryExpectingRefusal requires a node-pool
// configuration update to fail with 400 and returns the validation message.
func (c *Client) UpdateNodePoolSystemReservedMemoryExpectingRefusal(ctx context.Context, id openapi_types.UUID, memory string) (string, error) {
	resp, err := c.api.UpdateNodePoolWithResponse(ctx, id, api.UpdateNodePoolRequest{
		SystemReservedMemory: &memory,
	})
	if err != nil {
		return "", fmt.Errorf("updating node pool %s system-reserved memory: %w", id, err)
	}
	if resp.StatusCode() != 400 || resp.JSON400 == nil {
		return "", fmt.Errorf("updating node pool %s system-reserved memory to %q: want 400, got %s: %s", id, memory, resp.Status(), resp.Body)
	}
	return resp.JSON400.Message, nil
}

// DeleteNodeOption configures the DeleteNode request body.
type DeleteNodeOption func(*api.DeleteNodeRequest)

// WithForceUnreachable tolerates best-effort drain / graceful-shutdown failures
// for a node whose kubelet/agent is unresponsive.
func WithForceUnreachable() DeleteNodeOption {
	return func(r *api.DeleteNodeRequest) { r.ForceUnreachable = true }
}

func (c *Client) DeleteNode(ctx context.Context, id openapi_types.UUID, opts ...DeleteNodeOption) error {
	req := api.DeleteNodeRequest{}
	for _, opt := range opts {
		opt(&req)
	}
	resp, err := c.api.DeleteNodeWithResponse(ctx, id, req)
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
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	for i := range clusters {
		if clusters[i].Name == name {
			return &clusters[i], nil
		}
	}
	return nil, fmt.Errorf("cluster %q not found", name)
}

// WithDefaults fills in the timeouts a caller left unset. Exported so
// internal/nvkectl, which waits on the same operations through the CLI, waits
// exactly as long as this client would rather than keeping its own copy.
func WithDefaults(opts Options) Options {
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
