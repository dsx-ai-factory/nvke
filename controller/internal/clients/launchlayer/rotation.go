// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TryAdvanceStuckMachine is always a no-op: CAPLL has no controller-pinned
// candidate list to rotate through, and reimage is explicitly not used as an
// automatic repair action by CAPLL itself.
func (*Client) TryAdvanceStuckMachine(_ context.Context, _ shared.MachineAdvanceDeps, _ db.Node, _ db.NodeSet, _ *clusterv1.Machine) (bool, error) {
	return false, nil
}
