// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clients holds the provider-client factories used by reconciliation
// code, and their per-provider implementations in subpackages (capi, nico,
// and future providers). Each factory's only job is to select an
// implementation from the cluster/node's provider type; provider-specific
// behavior lives in that provider's own subpackage.
package clients

import (
	"encoding/json"
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ClusterScaffold builds the infrastructure-provider manifest for a cluster.
// Each provider type has its own implementation, selected by
// GetClusterScaffold, so provider-specific logic lives in its own package
// instead of behind a switch shared by every provider.
type ClusterScaffold interface {
	GetClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error)
}

// GetClusterScaffold selects the ClusterScaffold implementation for the
// cluster's provider type. Adding a provider means adding a case here and
// its own implementation subpackage, not a new switch arm at each call site.
func GetClusterScaffold(cluster db.Cluster) (ClusterScaffold, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}
