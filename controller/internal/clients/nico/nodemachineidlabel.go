// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// NodeMachineIDLabelValue returns node's Forge/Carbide machine ID, which is
// "" (no label) until the machine is allocated.
func (*Client) NodeMachineIDLabelValue(node db.Node) (string, error) {
	provider, err := node.NicoProvider()
	if err != nil {
		return "", err
	}
	return provider.ForgeMachineID, nil
}
