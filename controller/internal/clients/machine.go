// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// MachineMaterialization builds the provider-specific infrastructure machine
// object for a node: a generic CAPI infra machine rendered from a
// caller-supplied template, or a NicoMachine CWE builds itself. Each
// provider type has its own implementation, selected by
// GetMachineMaterialization.
type MachineMaterialization interface {
	// RenderInfraMachine builds the infra machine object for the node.
	// profilesDir is the OS/config profiles directory on disk; only the NICo
	// implementation reads from it (for the iPXE script).
	RenderInfraMachine(profilesDir, namespace string, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) (client.Object, error)
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
