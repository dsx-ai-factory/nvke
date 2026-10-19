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

// RebootAnnotationProvider resolves the annotation keys a hard reboot request
// must write onto the generic CAPI Machine object for the cluster's
// infrastructure provider to act on it. Each provider has its own
// implementation, selected by GetRebootAnnotationProvider, because the keys
// are not uniform: CAPNico watches a configurable key, while CAPLL watches a
// hardcoded key it defines itself.
type RebootAnnotationProvider interface {
	// HardRebootAnnotationKeys returns the Machine annotation keys that
	// trigger an out-of-band infrastructure-level reboot for this provider.
	// The same keys are applied to request a reboot and dropped to clear it.
	HardRebootAnnotationKeys() []string
}

// GetRebootAnnotationProvider selects the RebootAnnotationProvider
// implementation for the cluster's provider type. Adding a provider means
// adding a case here and its own implementation subpackage, not a new
// switch arm at each call site.
func GetRebootAnnotationProvider(cluster db.Cluster) (RebootAnnotationProvider, error) {
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
