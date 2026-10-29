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

// ControlPlaneConfigPayloadValidator validates a control-plane config
// snapshot's provider-specific payload content — not field presence
// relative to the HTTP request, which internal/server validates before
// building the payload this takes. Each provider type has its own
// implementation, selected by GetControlPlaneConfigPayloadValidator.
type ControlPlaneConfigPayloadValidator interface {
	// ValidateControlPlaneConfigPayload validates spec's provider-specific
	// payload.
	ValidateControlPlaneConfigPayload(spec db.ControlPlaneConfigSpec) error
}

// GetControlPlaneConfigPayloadValidator selects the
// ControlPlaneConfigPayloadValidator implementation for the cluster's
// provider type. Adding a provider means adding a case here and its own
// implementation subpackage, not a new switch arm at each call site.
func GetControlPlaneConfigPayloadValidator(cluster db.Cluster) (ControlPlaneConfigPayloadValidator, error) {
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
