// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateControlPlaneConfigPayload validates spec's CAPI payload content:
// the provisioned control-plane machine template, if present, must be a
// well-formed CAPI infrastructure provider manifest. Only the first entry of
// InfraMachineTemplatePreferences is the provisioned template — the rest is
// merge history and not part of the API.
func (*Client) ValidateControlPlaneConfigPayload(spec db.ControlPlaneConfigSpec) error {
	capiProvider, err := spec.CAPIProvider()
	if err != nil {
		return err
	}
	return ValidateInfrastructureManifestBytes(firstInfraMachineTemplate(capiProvider.InfraMachineTemplatePreferences))
}

// firstInfraMachineTemplate returns the provisioned control-plane machine
// template from the stored preference list (index 0), or nil if absent.
func firstInfraMachineTemplate(prefs []json.RawMessage) json.RawMessage {
	if len(prefs) == 0 {
		return nil
	}
	return prefs[0]
}
