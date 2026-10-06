// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// MachineMaterialization builds and creates the provider-specific
// infrastructure machine for a node: a generic CAPI infra machine rendered
// from a caller-supplied template, or a NicoMachine CWE builds itself. Each
// provider type has its own implementation, selected by
// GetMachineMaterialization.
type MachineMaterialization interface {
	// CreateMachine renders and applies the infra machine, bootstrap secret,
	// and generic CAPI Machine for the node, calling whichever of deps'
	// callbacks this provider needs.
	CreateMachine(ctx context.Context, deps shared.MachineCreateDeps, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) error
}

// GetMachineMaterialization selects the MachineMaterialization
// implementation for the cluster's provider type. Adding a provider means
// adding a case here and a RenderInfraMachine method on its client package,
// not a new switch arm at each call site.
func GetMachineMaterialization(cluster db.Cluster) (MachineMaterialization, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}
