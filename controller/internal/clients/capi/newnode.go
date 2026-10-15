// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyNodePoolNodePayload is a no-op: CAPI nodes carry no per-node
// provider payload the way NICo nodes do.
func (*Client) ApplyNodePoolNodePayload(_ db.NodePool, _ *db.Node) error {
	return nil
}

// ApplyPlaceholderNodePayload is a no-op: CAPI nodes carry no per-node
// provider payload the way NICo nodes do.
func (*Client) ApplyPlaceholderNodePayload(_ *db.Node) {}

// ApplyObservedNodeIdentity records Forge identity for the one CAPI case that
// reports it: a cluster whose own template renders a NicoMachine (see
// applyNicoMachineInstanceLabels). CAPD reports none, so nothing is stamped.
func (*Client) ApplyObservedNodeIdentity(node *db.Node, infraMachine *unstructured.Unstructured) error {
	return shared.StampForgeIdentity(node, infraMachine)
}
