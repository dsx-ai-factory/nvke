// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"fmt"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// NewNodeProvider stamps the provider-specific payload a newly created Node
// needs, so strict provider-payload readers (e.g. labelNode) succeed. Each
// provider type has its own implementation, selected by GetNewNodeProvider.
type NewNodeProvider interface {
	// ApplyNodePoolNodePayload sets node's provider-specific payload from
	// nodePool, for a node backed by a real node pool.
	ApplyNodePoolNodePayload(nodePool db.NodePool, node *db.Node) error

	// ApplyPlaceholderNodePayload sets a provider-specific placeholder
	// payload on node when creating a node not backed by any node pool
	// (e.g. a control-plane node created directly from cluster config), so
	// downstream strict provider-payload readers succeed. Any fields the
	// infra provider or a later activity settles (e.g. an instance type)
	// are left unset here.
	ApplyPlaceholderNodePayload(node *db.Node)
}

// GetNewNodeProvider selects the NewNodeProvider implementation for the
// cluster's provider type. Adding a provider means adding a case here and
// its own implementation subpackage, not a new switch arm at each call
// site.
func GetNewNodeProvider(cluster db.Cluster) (NewNodeProvider, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}
