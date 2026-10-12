// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"
	"log/slog"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// NodePoolUpgradeConfig resolves the release and/or KubernetesVersion a
// NodePool should move to when the cluster's target Kubernetes version
// changes. Each provider has its own implementation, selected by
// GetNodePoolUpgradeConfig, so provider-specific logic lives in its own
// package instead of behind a switch shared by every provider.
type NodePoolUpgradeConfig interface {
	// ResolveNodePoolUpgrade returns nodePool updated to target
	// targetKubernetesVersion, resolving any provider-specific fields (e.g. a
	// NICo release) the version change requires. reader reads the release
	// resources the resolution selects from.
	ResolveNodePoolUpgrade(
		ctx context.Context,
		reader ctrlclient.Reader,
		nodePool db.NodePool,
		targetKubernetesVersion string,
		logger *slog.Logger,
	) (db.NodePool, error)
}

// GetNodePoolUpgradeConfig selects the NodePoolUpgradeConfig implementation
// for the cluster's provider type. Adding a provider means adding a case
// here and its own implementation subpackage, not a new switch arm at each
// call site.
func GetNodePoolUpgradeConfig(cluster db.Cluster) (NodePoolUpgradeConfig, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for NodePool KubernetesVersion upgrade", cluster.ProviderType)
	}
}
