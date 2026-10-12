// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
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
