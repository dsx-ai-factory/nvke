// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/launchlayer"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TenantAPIAccess decides how the controller reaches a cluster's tenant
// Kubernetes API. Each provider type has its own implementation, selected by
// GetTenantAPIAccess. It only chooses the access method; building a client
// from that method is internal/kube's job.
type TenantAPIAccess interface {
	// TenantAPIAccessMethod returns how to reach a cluster's tenant API.
	// tenantAccessEnabled is the controller-wide rollout flag for consuming
	// the Secret TenantAccess publishes; a provider may ignore it.
	TenantAPIAccessMethod(tenantAccessEnabled bool) shared.TenantAPIAccessMethod
}

// GetTenantAPIAccess selects the TenantAPIAccess implementation for the
// cluster's provider type. Adding a provider means adding a case here and
// its own implementation subpackage, not a new switch arm at each call
// site.
func GetTenantAPIAccess(cluster db.Cluster) (TenantAPIAccess, error) {
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
