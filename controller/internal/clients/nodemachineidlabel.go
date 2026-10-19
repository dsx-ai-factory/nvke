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

// NodeMachineIDLabel resolves the value of the nvke.nvidia.com/machine-id
// label a provisioned node carries, so the provider's machine identity is
// discoverable on the Kubernetes Node object. Each provider type has its own
// implementation, selected by GetNodeMachineIDLabel.
//
// Implementations must stay pure functions of their arguments: labelNode
// calls this from Temporal workflow code, where any I/O would break replay.
type NodeMachineIDLabel interface {
	// NodeMachineIDLabelValue returns node's machine-ID label value, or ""
	// when the node should carry no machine-ID label.
	NodeMachineIDLabelValue(node db.Node) (string, error)
}

// GetNodeMachineIDLabel selects the NodeMachineIDLabel implementation for
// the cluster's provider type. Adding a provider means adding a case here
// and its own implementation subpackage, not a new switch arm at each call
// site.
func GetNodeMachineIDLabel(cluster db.Cluster) (NodeMachineIDLabel, error) {
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
