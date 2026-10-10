// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ApplyControlPlaneConfigPayloads sets nodeSet's and nodePool's CAPI payload
// from snapshotCfg.Capi's first infra-machine-template preference. node is
// unused — CAPI has no per-node control-plane field the way NICo's pinned
// instance type is.
func (*Client) ApplyControlPlaneConfigPayloads(_ db.Node, snapshotCfg db.ControlPlaneConfigSpec, nodeSet *db.NodeSet, nodePool *db.NodePool) {
	if snapshotCfg.Capi == nil {
		return
	}

	template := json.RawMessage(`{}`)
	if len(snapshotCfg.Capi.InfraMachineTemplatePreferences) > 0 {
		template = append(json.RawMessage(nil), snapshotCfg.Capi.InfraMachineTemplatePreferences[0]...)
	}

	payload := db.CAPINodePoolProvider{
		InfraMachineTemplate:   template,
		SpireAttestationMethod: snapshotCfg.Capi.SpireAttestationMethod,
		BootstrapFiles:         append([]db.CloudInitFile(nil), snapshotCfg.Capi.BootstrapFiles...),
		PreKubeadmCommands:     append([]string(nil), snapshotCfg.Capi.PreKubeadmCommands...),
	}
	nodeSet.SetCapi(payload)
	nodePool.SetCapi(payload)
}
