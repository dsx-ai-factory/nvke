// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// MachineMaterialization builds and creates the provider-specific
// infrastructure machine for a node: a generic CAPI infra machine rendered
// from a caller-supplied template, or a NicoMachine the controller builds itself. Each
// provider type has its own implementation, selected by
// GetMachineMaterialization.
type MachineMaterialization interface {
	// CreateMachine renders and applies the infra machine, bootstrap secret,
	// and generic CAPI Machine for the node, calling whichever of deps'
	// callbacks this provider needs.
	CreateMachine(ctx context.Context, deps shared.MachineCreateDeps, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) error

	// TryAdvanceStuckMachine inspects a Machine that has been waiting with no
	// IP address and, if the failure looks like a retryable unavailability,
	// advances it to the next pinned option (network candidate or instance
	// type). Returns true when it advanced (caller should keep waiting for
	// the new attempt); false when there was nothing to advance.
	TryAdvanceStuckMachine(ctx context.Context, deps shared.MachineAdvanceDeps, node db.Node, nodeSet db.NodeSet, machine *clusterv1.Machine) (bool, error)
}

// GetMachineMaterialization selects the MachineMaterialization
// implementation for the cluster's provider type. Adding a provider means
// adding a case here and an implementation in its own client package, not a
// new switch arm at each call site.
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
