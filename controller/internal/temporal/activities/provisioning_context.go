// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

func ResolveClusterInfraMode(cluster db.Cluster) (InfraMode, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		provider, err := cluster.CAPIProvider()
		if err != nil {
			return "", err
		}
		if capi.IsEmptyManifest(provider.InfraCluster) {
			return "", fmt.Errorf("cluster %s must define capiInfraCluster when provider type is %q", cluster.ID, db.ProviderTypeCAPI)
		}
		return InfraModeCAPI, nil
	case db.ProviderTypeNico:
		if _, err := cluster.NicoProvider(); err != nil {
			return "", err
		}
		return InfraModeCAPI, nil
	default:
		return "", fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}

func ResolveNodeInfraMode(cluster db.Cluster, nodeSet *db.NodeSet) (InfraMode, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		provider, err := cluster.CAPIProvider()
		if err != nil {
			return "", err
		}
		if capi.IsEmptyManifest(provider.InfraCluster) {
			return "", fmt.Errorf("cluster %s must define capiInfraCluster when provider type is %q", cluster.ID, db.ProviderTypeCAPI)
		}

		capiInfraMachineTemplate := []byte("{}")
		templateSource := "resolved node context"
		if nodeSet != nil {
			capiNodeSetProvider, err := nodeSet.CAPIProvider()
			if err != nil {
				return "", fmt.Errorf("failed to read node set capi provider: %w", err)
			}
			capiInfraMachineTemplate = capiNodeSetProvider.InfraMachineTemplate
			templateSource = fmt.Sprintf("node set %s", nodeSet.ID)
		}
		if capi.IsEmptyManifest(capiInfraMachineTemplate) {
			return "", fmt.Errorf("%s must define capiInfraMachineTemplate because cluster %s is CAPI-backed", templateSource, cluster.ID)
		}
		return InfraModeCAPI, nil
	case db.ProviderTypeNico:
		if _, err := cluster.NicoProvider(); err != nil {
			return "", err
		}
		return InfraModeCAPI, nil
	default:
		return "", fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}

func (a *Activities) GetClusterProvisioningContext(ctx context.Context, clusterID uuid.UUID) (ClusterProvisioningContext, error) {
	cluster, err := a.db.GetCluster(ctx, clusterID)
	if err != nil {
		return ClusterProvisioningContext{}, wrapProvisioningContextLookupError("cluster", err)
	}

	infraMode, err := ResolveClusterInfraMode(cluster)
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
	configProfile := ""
	if nodeSet.Nico != nil {
		nicoNodeSetProvider, err := nodeSet.NicoProvider()
		if err != nil {
			return NodeProvisioningContext{}, fmt.Errorf("failed to read node set nico provider: %w", err)
		}
		configProfile = nicoNodeSetProvider.ConfigProfile
	}

	infraMode, err := ResolveNodeInfraMode(cluster, &nodeSet)
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
