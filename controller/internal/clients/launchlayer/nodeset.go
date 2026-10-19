// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyNodeSetProvider sets newNodeSet's LaunchLayer payload from nodePool's.
func (*Client) ApplyNodeSetProvider(nodePool db.NodePool, newNodeSet *db.NodeSet) error {
	provider, err := nodePool.LaunchLayerProvider()
	if err != nil {
		return err
	}
	newNodeSet.SetLaunchLayer(provider)
	return nil
}
