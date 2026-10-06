// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"log/slog"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ResolveNodePoolUpgrade updates nodePool's KubernetesVersion only. CAPI
// node pools carry no OSProfile, so there is nothing else to resolve.
func (*Client) ResolveNodePoolUpgrade(_ string, nodePool db.NodePool, targetKubernetesVersion string, _ *slog.Logger) (db.NodePool, error) {
	nodePool.KubernetesVersion = targetKubernetesVersion
	return nodePool, nil
}
