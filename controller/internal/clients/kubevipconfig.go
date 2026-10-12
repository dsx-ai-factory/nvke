// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// KubeVIPConfig resolves provider-specific kube-vip configuration. Each
// provider has its own implementation, selected by GetKubeVIPConfig, so
// provider-specific logic lives in its own package instead of behind a
// switch shared by every provider.
type KubeVIPConfig interface {
	// UsesFMDSBGPPeerASN reports whether nodePool's provider-specific network
	// configuration means kube-vip should source its BGP peer ASN from FMDS
	// instead of using the static default.
	UsesFMDSBGPPeerASN(nodePool db.NodePool) (bool, error)
}

// GetKubeVIPConfig selects the KubeVIPConfig implementation for the
// cluster's provider type. Adding a provider means adding a case here and
// its own implementation subpackage, not a new switch arm at each call
// site.
func GetKubeVIPConfig(cluster db.Cluster) (KubeVIPConfig, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capi.New(), nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for kube-vip configuration", cluster.ProviderType)
	}
}
