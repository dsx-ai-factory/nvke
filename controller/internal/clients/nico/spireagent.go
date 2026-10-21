// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateNodeAgentProfile accepts every profile: NICo nodes support each
// SPIRE attestation method and transport.
func (*Client) ValidateNodeAgentProfile(db.Node, db.NodePool) error {
	return nil
}

// SpireAgentPath supplies no path: NicoSpiffeController writes a NICo
// Machine's agent ID path from NicoMachine.status.
func (*Client) SpireAgentPath(context.Context, client.Client, db.Node, *clusterv1.Machine) (string, error) {
	return "", nil
}
