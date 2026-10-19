// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/launchlayer"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// NodeAgentProfileValidator reports whether a node pool's nodes can join
// under a node-agent profile: the SPIFFE, SPIRE attestation and transport
// settings nodes.PinAgentProfile stamps on a new node. Each provider has its
// own implementation, selected by GetNodeAgentProfileValidator.
type NodeAgentProfileValidator interface {
	// ValidateNodeAgentProfile returns an error naming the setting the
	// provider cannot support for nodePool's nodes, or nil.
	ValidateNodeAgentProfile(profile db.Node, nodePool db.NodePool) error
}

// GetNodeAgentProfileValidator selects the NodeAgentProfileValidator
// implementation for the cluster's provider type. Adding a provider means
// adding a case here and its own implementation subpackage, not a new
// switch arm at each call site.
func GetNodeAgentProfileValidator(cluster db.Cluster) (NodeAgentProfileValidator, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	case db.ProviderTypeLaunchLayer:
		return launchlayer.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}
