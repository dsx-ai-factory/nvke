// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TryAdvanceStuckMachine moves a NicoMachine-backed control-plane node off a
// failure domain CAPNICo reports out of capacity, through deps.RotateFailureDomain.
// Other infrastructure comes from a caller-supplied template, with no
// controller-pinned candidate list to rotate through.
func (*Client) TryAdvanceStuckMachine(ctx context.Context, deps shared.MachineAdvanceDeps, node db.Node, _ db.NodeSet, _ db.NodePool, machine *clusterv1.Machine) (bool, error) {
	if machine.Spec.InfrastructureRef.APIGroup != nicov1.GroupVersion.Group || machine.Spec.InfrastructureRef.Kind != "NicoMachine" {
		return false, nil
	}
	return nico.RotateFailureDomain(ctx, deps, node, machine)
}

// DescribeStuckMachine always returns "": with nothing to advance, the
// infrastructure provider's own message is the most specific one.
func (*Client) DescribeStuckMachine(_ context.Context, _ shared.MachineAdvanceDeps, _ db.NodePool, _ *clusterv1.Machine) (string, error) {
	return "", nil
}
