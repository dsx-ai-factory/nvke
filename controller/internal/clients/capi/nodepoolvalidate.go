// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateNodePoolPayload validates nodePool's CAPI payload content: the
// infra machine template, if present, must be a well-formed CAPI
// infrastructure provider manifest.
func (*Client) ValidateNodePoolPayload(nodePool db.NodePool) error {
	capiNodePoolProvider, err := nodePool.CAPIProvider()
	if err != nil {
		return err
	}
	return ValidateInfrastructureManifestBytes(capiNodePoolProvider.InfraMachineTemplate)
}
