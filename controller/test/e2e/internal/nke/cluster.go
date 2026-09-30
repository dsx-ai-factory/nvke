// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/controller/api"
)

// GetCluster returns the cluster with the given id.
func (c *Client) GetCluster(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error) {
	resp, err := c.api.GetClusterWithResponse(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting cluster %s: %w", id, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("getting cluster %s: unexpected status %s: %s", id, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetClusterControlPlane returns the read-only control-plane view (persisted
// config plus aggregate runtime status) for the cluster with the given id.
func (c *Client) GetClusterControlPlane(ctx context.Context, id openapi_types.UUID) (*api.ClusterControlPlane, error) {
	resp, err := c.api.GetClusterControlPlaneWithResponse(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting control plane for cluster %s: %w", id, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("getting control plane for cluster %s: unexpected status %s: %s", id, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// UpgradeClusterControlPlane requests a control-plane Kubernetes version upgrade
// and enables control-plane node recycling.
func (c *Client) UpgradeClusterControlPlane(ctx context.Context, id openapi_types.UUID, version string) (*api.Cluster, error) {
	enableRecycling := true
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		ControlPlaneKubernetesVersion: &version,
		EnableCPNodeRecycling:         &enableRecycling,
	})
	if err != nil {
		return nil, fmt.Errorf("upgrading cluster %s control plane to %s: %w", id, version, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("upgrading cluster %s control plane to %s: unexpected status %s: %s", id, version, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// UpgradeClusterControlPlaneExpectingRefusal sends a version and maxSurge update,
// requires HTTP 400, and returns the validation message.
func (c *Client) UpgradeClusterControlPlaneExpectingRefusal(ctx context.Context, id openapi_types.UUID, version string) (string, error) {
	enableRecycling := true
	maxSurge := 1
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		ControlPlaneKubernetesVersion: &version,
		EnableCPNodeRecycling:         &enableRecycling,
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			MaxSurge: &maxSurge,
		},
	})
	if err != nil {
		return "", fmt.Errorf("upgrading cluster %s control plane to %s: %w", id, version, err)
	}
	if resp.StatusCode() != 400 || resp.JSON400 == nil {
		return "", fmt.Errorf("upgrading cluster %s control plane to %s: want 400, got %s: %s", id, version, resp.Status(), resp.Body)
	}
	return resp.JSON400.Message, nil
}

// UpdateControlPlaneDesiredCount patches the cluster-level control-plane config
// to the given desired count and returns the updated cluster. It is used by
// control-plane-controller provisioning, where the control plane is managed
// from the cluster-level controlPlaneConfig rather than a control-plane node
// pool, so the desired count cannot be changed through the node pool API. The
// generated UpdateClusterControlPlaneConfig carries the writable subset of the
// control-plane config, so no additional request type is needed here.
func (c *Client) UpdateControlPlaneDesiredCount(ctx context.Context, id openapi_types.UUID, desired int) (*api.Cluster, error) {
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("updating cluster %s control-plane desired count to %d: %w", id, desired, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("updating cluster %s control-plane desired count to %d: unexpected status %s: %s", id, desired, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateControlPlaneDesiredCountExpectingRefusal requires a cluster-level
// desired-count update to fail with 400 and returns the validation message.
func (c *Client) UpdateControlPlaneDesiredCountExpectingRefusal(ctx context.Context, id openapi_types.UUID, desired int) (string, error) {
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			DesiredCount: &desired,
		},
	})
	if err != nil {
		return "", fmt.Errorf("updating cluster %s control-plane desired count to %d: %w", id, desired, err)
	}
	if resp.StatusCode() != 400 || resp.JSON400 == nil {
		return "", fmt.Errorf("updating cluster %s control-plane desired count to %d: want 400, got %s: %s", id, desired, resp.Status(), resp.Body)
	}
	return resp.JSON400.Message, nil
}

// UpdateControlPlaneConfigProfile patches the cluster-level control-plane config
// profile and enables control-plane node recycling, returning the updated
// cluster. Changing the config profile bumps the control-plane configuration
// revision, which the control-plane controller rolls onto replacement nodes; the
// rolling teardown only runs when control-plane node recycling is enabled. It is
// used by control-plane-controller provisioning, where the control plane has no
// owning node pool and so cannot be rolled through the node pool API.
func (c *Client) UpdateControlPlaneConfigProfile(ctx context.Context, id openapi_types.UUID, configProfile string) (*api.Cluster, error) {
	enableRecycling := true
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		EnableCPNodeRecycling: &enableRecycling,
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{
				ConfigProfile: &configProfile,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("updating cluster %s control-plane config profile to %q: %w", id, configProfile, err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("updating cluster %s control-plane config profile to %q: unexpected status %s: %s", id, configProfile, resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateEtcdEncryption updates whether etcd encryption is enabled for a cluster.
func (c *Client) UpdateEtcdEncryption(ctx context.Context, id openapi_types.UUID, enable bool) (*api.UpdateClusterClientResponse, error) {
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		EnableEtcdEncryption: &enable,
	})
	if err != nil {
		return nil, fmt.Errorf("updating etcd encryption for cluster %s to %t: %w", id, enable, err)
	}
	return resp, nil
}

// UpdateOIDC updates whether OIDC authentication is enabled for a cluster.
func (c *Client) UpdateOIDC(ctx context.Context, id openapi_types.UUID, enable bool) (*api.UpdateClusterClientResponse, error) {
	resp, err := c.api.UpdateClusterWithResponse(ctx, id, api.UpdateClusterRequest{
		EnableOidc: &enable,
	})
	if err != nil {
		return nil, fmt.Errorf("updating OIDC for cluster %s to %t: %w", id, enable, err)
	}
	return resp, nil
}
