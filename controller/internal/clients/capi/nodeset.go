// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyNodeSetProvider sets newNodeSet's CAPI payload from nodePool's.
func (*Client) ApplyNodeSetProvider(nodePool db.NodePool, newNodeSet *db.NodeSet) error {
	capiNodePoolProvider, err := nodePool.CAPIProvider()
	if err != nil {
		return err
	}
	newNodeSet.SetCapi(capiNodePoolProvider)
	return nil
}
