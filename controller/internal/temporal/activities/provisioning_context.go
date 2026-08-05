// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type InfraMode string

const (
	InfraModeForge InfraMode = "forge"
	InfraModeCAPI  InfraMode = "capi"
)

type ClusterProvisioningContext struct {
	ClusterID uuid.UUID
	InfraMode InfraMode

	// ClusterName lets teardown reference name-keyed resources after the cluster
	// row is gone.
	ClusterName string
}

type NodeProvisioningContext struct {
	NodeID            uuid.UUID
	InfraMode         InfraMode
	ConfigProfile     string
	ManagedByNodePool bool // True when the node is managed by a node pool (has a non-nil node pool ID).
}

func ResolveClusterInfraMode(cluster db.Cluster, enableForgeCapi bool) (InfraMode, error) {
	if cluster.SiteID == uuid.Nil {
		if capi.IsEmptyManifest(cluster.CapiInfraCluster) {
			return "", fmt.Errorf("cluster %s must define capiInfraCluster when siteID is the nil UUID", cluster.ID)
		}

		return InfraModeCAPI, nil
	}
	if enableForgeCapi && !capi.IsEmptyManifest(cluster.CapiInfraCluster) {
		return InfraModeCAPI, nil
	}

	return InfraModeForge, nil
}

func ResolveNodeInfraMode(cluster db.Cluster, nodeSet *db.NodeSet, enableForgeCapi bool) (InfraMode, error) {
	capiInfraMachineTemplate := []byte("{}")
	templateSource := "resolved node context"
	if nodeSet != nil {
		capiInfraMachineTemplate = nodeSet.CapiInfraMachineTemplate
		templateSource = fmt.Sprintf("node set %s", nodeSet.ID)
	}

	if cluster.SiteID == uuid.Nil {
		if capi.IsEmptyManifest(cluster.CapiInfraCluster) {
			return "", fmt.Errorf("cluster %s must define capiInfraCluster when siteID is the nil UUID", cluster.ID)
		}
		if capi.IsEmptyManifest(capiInfraMachineTemplate) {
			return "", fmt.Errorf("%s must define capiInfraMachineTemplate because cluster %s is CAPI-backed", templateSource, cluster.ID)
		}

		return InfraModeCAPI, nil
	}

	if capi.ShouldUseCAPIForNodeProvisioning(enableForgeCapi, cluster) {
		return InfraModeCAPI, nil
	}

	return InfraModeForge, nil
}

func (a *Activities) GetClusterProvisioningContext(ctx context.Context, clusterID uuid.UUID) (ClusterProvisioningContext, error) {
	cluster, err := a.db.GetCluster(ctx, clusterID)
	if err != nil {
		return ClusterProvisioningContext{}, wrapProvisioningContextLookupError("cluster", err)
	}

	infraMode, err := ResolveClusterInfraMode(cluster, a.cfg.EnableForgeCapi)
	if err != nil {
		return ClusterProvisioningContext{}, fmt.Errorf("failed to resolve cluster infra mode: %w", err)
	}

	return ClusterProvisioningContext{
		ClusterID:   cluster.ID,
		InfraMode:   infraMode,
		ClusterName: cluster.Name,
	}, nil
}

func (a *Activities) GetNodeProvisioningContext(ctx context.Context, nodeID uuid.UUID) (NodeProvisioningContext, error) {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return NodeProvisioningContext{}, wrapProvisioningContextLookupError("node", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return NodeProvisioningContext{}, wrapProvisioningContextLookupError("cluster", err)
	}

	nodeSet, _, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return NodeProvisioningContext{}, fmt.Errorf("failed to resolve node context: %w", err)
	}
	configProfile := nodeSet.ConfigProfile

	infraMode, err := ResolveNodeInfraMode(cluster, &nodeSet, a.cfg.EnableForgeCapi)
	if err != nil {
		return NodeProvisioningContext{}, fmt.Errorf("failed to resolve node infra mode: %w", err)
	}

	return NodeProvisioningContext{
		NodeID:            node.ID,
		InfraMode:         infraMode,
		ConfigProfile:     configProfile,
		ManagedByNodePool: node.IsManagedByNodePool(),
	}, nil
}

func wrapProvisioningContextLookupError(resource string, err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("%s not found in database", resource), "db.ErrNotFound", err)
	}

	return fmt.Errorf("failed to get %s: %w", resource, err)
}
