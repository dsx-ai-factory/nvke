// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/launchlayer"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// SpireAgentPathProvider resolves the SPIRE agent ID path a node's agent
// attests as, which MachineSpireEntryController needs on the CAPI Machine
// before the node joins. Each provider has its own implementation, selected
// by GetSpireAgentPathProvider.
type SpireAgentPathProvider interface {
	// SpireAgentPath returns the agent ID path for node, backed by machine.
	// An empty path means this capability does not supply one for the node.
	// An error wrapping shared.ErrSpireAgentPathPending means the provider
	// has not yet reported what the path is built from.
	SpireAgentPath(ctx context.Context, k8sClient client.Client, node db.Node, machine *clusterv1.Machine) (string, error)
}

// GetSpireAgentPathProvider selects the SpireAgentPathProvider
// implementation for the cluster's provider type. Adding a provider means
// adding a case here and its own implementation subpackage, not a new
// switch arm at each call site.
func GetSpireAgentPathProvider(cluster db.Cluster) (SpireAgentPathProvider, error) {
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
