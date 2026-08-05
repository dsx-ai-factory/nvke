// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nke

import (
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/controller/api"
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
