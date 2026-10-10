// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ApplyNodePoolNodePayload sets node's NICo payload to the instance type
// pinned on nodePool.
func (*Client) ApplyNodePoolNodePayload(nodePool db.NodePool, node *db.Node) error {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return err
	}
	node.SetNico(db.NicoNodeProvider{InstanceTypeID: nicoNodePoolProvider.InstanceTypeID})
	return nil
}

// ApplyPlaceholderNodePayload sets an empty NICo payload on node so
// downstream strict provider-payload readers succeed. The instance type is
// left unset: the infra provider settles it, and waitForCAPIMachineReady
// records it.
func (*Client) ApplyPlaceholderNodePayload(node *db.Node) {
	node.SetNico(db.NicoNodeProvider{})
}
