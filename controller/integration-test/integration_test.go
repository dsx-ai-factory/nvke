// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration_test

// This suite drives a RUNNING CWE deployment (see README.md in this
// directory). Without a build tag it is picked up by a plain
// `go test ./...`, which then fails on a connection refused to
// localhost:8080 and names neither the cause nor the fix. `make test`
// only avoided it by filtering the package name out of `go list`.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/api"
)

type IntegrationTestSuite struct {
	config                IntegrationTestConfig
	client                *api.ClientWithResponses
	createClusterRequest  api.CreateClusterRequest
	createNodePoolRequest api.CreateNodePoolRequest

	// Test state
	clusterID        *openapi_types.UUID
	nodePoolID       *openapi_types.UUID
	desiredNodeCount *int
}

func TestIntegration(t *testing.T) {
	config := env.Must(loadIntegrationTestConfig())

	client, err := api.NewClientWithResponses(config.ControllerAPIURL)
	require.NoError(t, err, "Failed to create API client")

	createClusterRequest, err := loadCreateClusterRequest(config.CreateClusterRequestPath)
	require.NoError(t, err, "Failed to load create cluster request")

	createNodePoolRequest, err := loadCreateNodePoolRequest(config.CreateNodePoolRequestPath)
	require.NoError(t, err, "Failed to load create node pool request")

	suite := &IntegrationTestSuite{
		config:                config,
		client:                client,
		createClusterRequest:  createClusterRequest,
		createNodePoolRequest: createNodePoolRequest,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()

	// Always cleanup, even on failure (unless explicitly skipped)
	defer func() {
		if !config.SkipCleanup && suite.clusterID != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 1*time.Hour)
			defer cleanupCancel()
			t.Log("Cleaning up cluster...")
			suite.deleteCluster(t, cleanupCtx)
			suite.waitForClusterDeleted(t, cleanupCtx)
		}
	}()

	t.Run("E2E Cluster Lifecycle", func(t *testing.T) {
		suite.createCluster(t, ctx)
		suite.waitForClusterProvisioned(t, ctx)
		suite.addNodePool(t, ctx)
		suite.waitForNodePoolProvisioned(t, ctx)
		suite.updateNodePoolDesiredCount(t, ctx)
		suite.waitForNodesProvisioned(t, ctx)
	})
}

// loadCreateClusterRequest loads create cluster request from JSON file
func loadCreateClusterRequest(path string) (api.CreateClusterRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("failed to read create cluster request file '%s': %w", path, err)
	}

	var createClusterRequest api.CreateClusterRequest
	if err := json.Unmarshal(data, &createClusterRequest); err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("failed to unmarshal create cluster request from '%s': %w", path, err)
	}

	return createClusterRequest, nil
}

// loadCreateNodePoolRequest loads create node pool request from JSON file
func loadCreateNodePoolRequest(path string) (api.CreateNodePoolRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return api.CreateNodePoolRequest{}, fmt.Errorf("failed to read create node pool request file '%s': %w", path, err)
	}

	var createNodePoolRequest api.CreateNodePoolRequest
	if err := json.Unmarshal(data, &createNodePoolRequest); err != nil {
		return api.CreateNodePoolRequest{}, fmt.Errorf("failed to unmarshal create node pool request from '%s': %w", path, err)
	}

	return createNodePoolRequest, nil
}

func (s *IntegrationTestSuite) createCluster(t *testing.T, ctx context.Context) {
	t.Helper()
	// Append timestamp to cluster name for uniqueness
	timestamp := time.Now().Format("20060102-150405")

	createReq := s.createClusterRequest
	createReq.Name = fmt.Sprintf("%s-%s", s.createClusterRequest.Name, timestamp)
	if s.config.ControlPlaneVIP != "" {
		createReq.ControlPlaneVIP = s.config.ControlPlaneVIP
	}

	t.Logf("Creating cluster: %s", createReq.Name)

	resp, err := s.client.CreateClusterWithResponse(ctx, createReq)
	require.NoError(t, err, "Failed to create cluster")
	require.Equal(t, 201, resp.StatusCode(), "Create cluster failed: %s body: %s", resp.Status(), resp.Body)
	require.NotNil(t, resp.JSON201, "Create cluster response body is nil")

	s.clusterID = &resp.JSON201.ID
	t.Logf("Created cluster with ID: %s", resp.JSON201.ID)
}

func (s *IntegrationTestSuite) waitForClusterProvisioned(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NotNil(t, s.clusterID, "Cluster ID is nil")

	startTime := time.Now()
	timeout := time.After(75 * time.Minute)
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	t.Logf("Waiting for cluster %s to be provisioned...", *s.clusterID)

	for {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for cluster %s to be provisioned", *s.clusterID)
		case <-ctx.Done():
			t.Fatalf("Context canceled while waiting for cluster %s", *s.clusterID)
		case <-ticker.C:
			elapsed := time.Since(startTime).Round(time.Second)
			resp, err := s.client.GetClusterWithResponse(ctx, *s.clusterID)
			require.NoError(t, err, "Failed to get cluster")

			if resp.StatusCode() == 200 && resp.JSON200 != nil {
				cluster := resp.JSON200

				if cluster.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					t.Logf("Cluster %s is provisioned successfully (elapsed: %v)", cluster.Name, elapsed)
					return
				}

				if cluster.Conditions.Provisioning.Status == api.ConditionStatusFalse {
					t.Fatalf("Cluster %s provisioning failed: %s", cluster.Name, cluster.Conditions.Provisioning.Message)
				}

				t.Logf("Cluster %s is still provisioning: %s (elapsed: %v)", cluster.Name, cluster.Conditions.Provisioning.Message, elapsed)
			} else {
				t.Logf("Unexpected status code %d when checking cluster %s (elapsed: %v)", resp.StatusCode(), *s.clusterID, elapsed)
			}
		}
	}
}

func (s *IntegrationTestSuite) addNodePool(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NotNil(t, s.clusterID, "Cluster ID is nil")

	t.Logf("Adding worker node pool to cluster %s", *s.clusterID)

	// Set the cluster ID in the request
	createReq := s.createNodePoolRequest
	createReq.ClusterID = *s.clusterID
	s.desiredNodeCount = &createReq.DesiredNodeCount

	resp, err := s.client.CreateNodePoolWithResponse(ctx, createReq)
	require.NoError(t, err, "Failed to create node pool")
	require.Equal(t, 201, resp.StatusCode(), "Create node pool failed: %s body: %s", resp.Status(), resp.Body)
	require.NotNil(t, resp.JSON201, "Create node pool response body is nil")

	s.nodePoolID = &resp.JSON201.ID
	t.Logf("Created node pool with ID: %s", resp.JSON201.ID)
}

func (s *IntegrationTestSuite) waitForNodePoolProvisioned(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NotNil(t, s.nodePoolID, "Node pool ID is nil")

	startTime := time.Now()
	timeout := time.After(15 * time.Minute)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	t.Logf("Waiting for node pool %s to be provisioned...", *s.nodePoolID)

	for {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for node pool %s to be provisioned", *s.nodePoolID)
		case <-ctx.Done():
			t.Fatalf("Context canceled while waiting for node pool %s", *s.nodePoolID)
		case <-ticker.C:
			elapsed := time.Since(startTime).Round(time.Second)
			resp, err := s.client.GetNodePoolWithResponse(ctx, *s.nodePoolID)
			require.NoError(t, err, "Failed to get node pool")

			if resp.StatusCode() == 200 && resp.JSON200 != nil {
				nodePool := resp.JSON200

				if nodePool.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					t.Logf("Node pool %s is provisioned successfully (elapsed: %v)", nodePool.Name, elapsed)
					return
				}

				if nodePool.Conditions.Provisioning.Status == api.ConditionStatusFalse {
					t.Fatalf("Node pool %s provisioning failed: %s", nodePool.Name, nodePool.Conditions.Provisioning.Message)
				}

				t.Logf("Node pool %s is still provisioning: %s (elapsed: %v)", nodePool.Name, nodePool.Conditions.Provisioning.Message, elapsed)
			} else {
				t.Logf("Unexpected status code %d when checking node pool %s (elapsed: %v)", resp.StatusCode(), *s.nodePoolID, elapsed)
			}
		}
	}
}

func (s *IntegrationTestSuite) updateNodePoolDesiredCount(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NotNil(t, s.nodePoolID, "Node pool ID is nil")

	t.Logf("Adding node to pool: %s", *s.nodePoolID)

	desiredCount := 1
	updateReq := api.UpdateNodePoolRequest{
		DesiredNodeCount: &desiredCount,
	}

	resp, err := s.client.UpdateNodePoolWithResponse(ctx, *s.nodePoolID, updateReq)
	require.NoError(t, err, "Failed to update node pool")
	require.Equal(t, 200, resp.StatusCode(), "Update node pool failed: %s", resp.Status())
	require.NotNil(t, resp.JSON200, "Update node pool response body is nil")
	require.Equal(t, desiredCount, resp.JSON200.DesiredNodeCount, "Desired node count mismatch")

	s.desiredNodeCount = &desiredCount
	t.Logf("Updated node pool %s desired count to %d", *s.nodePoolID, desiredCount)
}

func (s *IntegrationTestSuite) waitForNodesProvisioned(t *testing.T, ctx context.Context) {
	t.Helper()
	startTime := time.Now()
	timeout := time.After(75 * time.Minute)
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	t.Logf("Waiting for %d node(s) to be provisioned for node pool %s...", *s.desiredNodeCount, *s.nodePoolID)

	for {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for %d node(s) to be provisioned for node pool %s", *s.desiredNodeCount, *s.nodePoolID)
		case <-ctx.Done():
			t.Fatalf("Context canceled while waiting for %d node(s) to be provisioned for node pool %s", *s.desiredNodeCount, *s.nodePoolID)
		case <-ticker.C:
			elapsed := time.Since(startTime).Round(time.Second)
			resp, err := s.client.ListNodesWithResponse(ctx, &api.ListNodesParams{
				NodePoolID: s.nodePoolID,
			})
			require.NoError(t, err, "Failed to list nodes")
			require.Equal(t, 200, resp.StatusCode(), "List nodes failed: %s", resp.Status())
			require.NotNil(t, resp.JSON200, "List nodes response body is nil")

			provisionedNodeCount := 0
			for _, node := range resp.JSON200.Nodes {
				if node.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					provisionedNodeCount++
				}
			}

			if provisionedNodeCount >= *s.desiredNodeCount {
				t.Logf("All %d / %d node(s) are provisioned for node pool %s (elapsed: %v)", provisionedNodeCount, *s.desiredNodeCount, *s.nodePoolID, elapsed)
				require.Equal(t, *s.desiredNodeCount, provisionedNodeCount, "Provisioned node count mismatch") // Detect a bug that creates too many nodes
				return
			}

			t.Logf("Waiting for %d / %d node(s) to be provisioned for node pool %s (elapsed: %v)", provisionedNodeCount, *s.desiredNodeCount, *s.nodePoolID, elapsed)
		}
	}
}

func (s *IntegrationTestSuite) deleteCluster(t *testing.T, ctx context.Context) {
	t.Helper()
	resp, err := s.client.DeleteClusterWithResponse(ctx, *s.clusterID)
	require.NoError(t, err, "Failed to delete cluster")
	require.Equal(t, 202, resp.StatusCode(), "Delete cluster failed: %s", resp.Status())
	t.Logf("Initiated deletion of cluster %s", *s.clusterID)
}

func (s *IntegrationTestSuite) waitForClusterDeleted(t *testing.T, ctx context.Context) {
	t.Helper()
	startTime := time.Now()
	timeout := time.After(40 * time.Minute)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	t.Logf("Waiting for cluster %s to be deleted...", *s.clusterID)

	for {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for cluster %s to be deleted", *s.clusterID)
		case <-ctx.Done():
			t.Fatalf("Context canceled while waiting for cluster %s", *s.clusterID)
		case <-ticker.C:
			elapsed := time.Since(startTime).Round(time.Second)
			resp, err := s.client.GetClusterWithResponse(ctx, *s.clusterID)
			if err != nil {
				t.Logf("Network error checking cluster %s status: %v (elapsed: %v)", *s.clusterID, err, elapsed)
				continue
			}

			if resp.StatusCode() == 404 {
				t.Logf("Cluster %s has been deleted (elapsed: %v)", *s.clusterID, elapsed)
				return
			}

			if resp.StatusCode() == 200 && resp.JSON200 != nil {
				cluster := resp.JSON200

				if cluster.Conditions.Deleting.Status == api.ConditionStatusFalse {
					t.Fatalf("Cluster %s deletion failed: %s", cluster.Name, cluster.Conditions.Deleting.Message)
				}

				t.Logf("Cluster %s is still deleting: %s (elapsed: %v)",
					cluster.Name, cluster.Conditions.Deleting.Message, elapsed)
			} else {
				t.Logf("Unexpected status code %d when checking cluster %s (elapsed: %v)", resp.StatusCode(), *s.clusterID, elapsed)
			}
		}
	}
}
