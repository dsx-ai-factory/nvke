// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// PinAgentProfile records the deployment settings at node creation. Later
// reconciliation and agent calls must not reinterpret this node using Helm's
// current defaults.
func PinAgentProfile(node *db.Node, cfg config.Config) {
	node.SpiffeEnabled = cfg.SpiffeEnabled
	node.SpireAttestationMethod = db.SpireAttestationMethod(cfg.NodeAgent.SpireAttestationMethod)
	if node.SpireAttestationMethod == "" {
		node.SpireAttestationMethod = db.SpireAttestationMethodTPM
	}
	node.NodeAgentExternalMTLSEnabled = cfg.NodeAgentExternalMTLSEnabled
	node.NodeAgentTLSJWTEnabled = cfg.NodeAgent.TLSJWT.Enabled
}
