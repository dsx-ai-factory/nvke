// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"fmt"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ControlPlaneConfigNodeProvider builds the provider-specific payload for a
// synthetic control-plane NodeSet/NodePool pair derived from a cluster's
// control-plane-config snapshot (used when a control-plane node has no real
// node pool/node set of its own). Each provider type has its own
// implementation, selected by GetControlPlaneConfigNodeProvider.
type ControlPlaneConfigNodeProvider interface {
	// ApplyControlPlaneConfigPayloads sets nodeSet's and nodePool's
	// provider-specific payload from snapshotCfg (and node, for the parts
	// that vary per node, e.g. a NICo pinned instance type). No-op if
	// snapshotCfg carries no payload for this provider.
	ApplyControlPlaneConfigPayloads(node db.Node, snapshotCfg db.ControlPlaneConfigSpec, nodeSet *db.NodeSet, nodePool *db.NodePool)
}

// GetControlPlaneConfigNodeProvider selects the ControlPlaneConfigNodeProvider
// implementation for the cluster's provider type. Adding a provider means
// adding a case here and its own implementation subpackage, not a new
// switch arm at each call site.
func GetControlPlaneConfigNodeProvider(cluster db.Cluster) (ControlPlaneConfigNodeProvider, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}
