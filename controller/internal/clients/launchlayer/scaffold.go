// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package launchlayer implements the LaunchLayer provider's capability
// clients. CAPLL's Go module (github.com/NVIDIA/cluster-api-provider-launchlayer)
// is not yet resolvable through the public Go module proxy, so this package
// builds LaunchLayerCluster/LaunchLayerMachine/LaunchLayerMachineTemplate
// manifests as plain JSON objects rather than importing CAPLL's typed API —
// the same approach internal/clients/capi already uses for a caller-supplied
// manifest, applied here to a controller-constructed one instead.
package launchlayer

import (
	"encoding/json"
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// Client is the LaunchLayer provider's capability client.
type Client struct{}

// New returns a LaunchLayer provider Client.
func New() *Client {
	return &Client{}
}

const (
	// infrastructureGroup and infrastructureAPIVersion are the CAPLL CRDs'
	// API group and apiVersion.
	infrastructureGroup      = "infrastructure.cluster.x-k8s.io"
	infrastructureAPIVersion = infrastructureGroup + "/v1alpha1"
	// nameKey is the metadata/security-group "name" JSON field, shared across
	// every manifest this package builds.
	nameKey = "name"
)

// GetClusterScaffoldManifest builds the LaunchLayerCluster manifest for a
// LaunchLayer-backed cluster, natively constructed from the cluster's
// LaunchLayer provider payload — the same construction style NICo uses,
// not a caller-supplied passthrough.
func (*Client) GetClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error) {
	provider, err := cluster.LaunchLayerProvider()
	if err != nil {
		return nil, err
	}

	spec := map[string]any{}
	if provider.VpcID != "" {
		spec["vpcID"] = provider.VpcID
	}
	if provider.OrgID != "" {
		spec["orgID"] = provider.OrgID
	}
	if provider.VpcType != "" {
		spec["vpcType"] = provider.VpcType
	}
	if provider.SiteID != "" {
		spec["siteID"] = provider.SiteID
	}
	if provider.CloudProviderEnabled {
		spec["cloudProviderEnabled"] = true
	}
	if owner := buildOwner(provider.Owner); owner != nil {
		spec["owner"] = owner
	}
	if len(provider.SecurityGroups) > 0 {
		spec["securityGroups"] = buildSecurityGroups(provider.SecurityGroups)
	}

	manifest := map[string]any{
		"apiVersion": infrastructureAPIVersion,
		"kind":       "LaunchLayerCluster",
		"metadata": map[string]any{
			nameKey: cluster.Name,
		},
		"spec": spec,
	}

	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to build LaunchLayerCluster manifest for cluster %s: %w", cluster.ID, err)
	}
	return raw, nil
}

func buildOwner(owner db.LaunchLayerClusterOwner) map[string]any {
	if owner.UserID == "" && owner.Username == "" {
		return nil
	}
	out := map[string]any{}
	if owner.UserID != "" {
		out["userID"] = owner.UserID
	}
	if owner.Username != "" {
		out["username"] = owner.Username
	}
	return out
}

func buildSecurityGroups(groups []db.LaunchLayerSecurityGroup) []map[string]any {
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		group := map[string]any{nameKey: g.Name, "type": g.Type}
		if g.NetworkType != "" {
			group["networkType"] = g.NetworkType
		}
		out = append(out, group)
	}
	return out
}
