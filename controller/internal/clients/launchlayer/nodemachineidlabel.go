// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// NodeMachineIDLabelValue returns the physical machine ID CAPLL recorded for
// node, which is "" (no label) for a virtual machine and until the instance
// is placed.
func (*Client) NodeMachineIDLabelValue(node db.Node) (string, error) {
	provider, err := node.LaunchLayerProvider()
	if err != nil {
		return "", err
	}
	return provider.MachineID, nil
}
