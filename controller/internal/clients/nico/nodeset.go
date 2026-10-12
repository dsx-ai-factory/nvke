// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyNodeSetProvider sets newNodeSet's NICo payload from nodePool's.
func (*Client) ApplyNodeSetProvider(nodePool db.NodePool, newNodeSet *db.NodeSet) error {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return err
	}
	newNodeSet.SetNico(nicoNodePoolProvider)
	return nil
}
