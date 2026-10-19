// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
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

// ApplyObservedNodeIdentity records the Forge instance and machine IDs
// NicoMachine.status reports.
func (*Client) ApplyObservedNodeIdentity(node *db.Node, infraMachine *unstructured.Unstructured) error {
	return shared.StampForgeIdentity(node, infraMachine)
}
