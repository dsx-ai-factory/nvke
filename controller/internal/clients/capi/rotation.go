// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// TryAdvanceStuckMachine is always a no-op: a template-backed CAPI machine's
// infrastructure comes from a caller-supplied template, with no controller-pinned
// candidate list to rotate through.
func (*Client) TryAdvanceStuckMachine(_ context.Context, _ shared.MachineAdvanceDeps, _ db.Node, _ db.NodeSet, _ *clusterv1.Machine) (bool, error) {
	return false, nil
}
