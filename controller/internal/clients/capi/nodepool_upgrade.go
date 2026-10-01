// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"log/slog"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ResolveNodePoolUpgrade updates nodePool's KubernetesVersion only. CAPI
// node pools carry no release reference, so there is nothing else to resolve.
func (*Client) ResolveNodePoolUpgrade(
	_ context.Context,
	_ ctrlclient.Reader,
	nodePool db.NodePool,
	targetKubernetesVersion string,
	_ *slog.Logger,
) (db.NodePool, error) {
	nodePool.KubernetesVersion = targetKubernetesVersion
	return nodePool, nil
}
