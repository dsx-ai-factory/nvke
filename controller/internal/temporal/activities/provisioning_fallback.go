// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// provisioningFallback advances a stuck CAPI Machine (one with no IP yet) to the
// next pinned provisioning option when the current attempt failed for a reason
// CWE can retry with a different option (instance type or network candidate). It
// centralizes the try-and-fallback concept so the mechanism can differ per
// provisioning path.
type provisioningFallback interface {
	// tryNext inspects the stuck machine and, if its failure is a retryable
	// unavailability, advances it to the next pinned option. Returns true when
	// it advanced (caller keeps waiting for the new attempt); false when there
	// was nothing to advance.
	tryNext(ctx context.Context, node db.Node, nodeSet db.NodeSet, machine *clusterv1.Machine) (bool, error)
}

// provisioningFallbackFor selects the fallback strategy by provisioning path.
// Templateless (Nico Forge-CAPI) nodes advance in place by patching the
// NicoMachine spec. Template-backed nodes render from their pinned
// InfraMachineTemplate and have no rotation fallback, so nil is returned.
func (a *Activities) provisioningFallbackFor(nodeSet db.NodeSet) provisioningFallback {
	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	if err != nil || capi.IsEmptyManifest(capiNodeSetProvider.InfraMachineTemplate) {
		return &nextInstanceTypeFallback{a: a}
	}
	return nil
}

// nextInstanceTypeFallback advances a templateless NicoMachine in place: it tries
// the network-candidate rotation first, then the instance-type rotation. The
// instance-type walk is cyclic (rotateNicoMachineInstanceType), so a node whose
// preferred types are all unavailable keeps probing them in preference order and
// converges on the highest-preference type once capacity returns.
type nextInstanceTypeFallback struct{ a *Activities }

func (f *nextInstanceTypeFallback) tryNext(ctx context.Context, node db.Node, nodeSet db.NodeSet, machine *clusterv1.Machine) (bool, error) {
	shouldRotateNet, err := f.a.shouldRotateNicoMachineNetworkCandidate(ctx, machine)
	if err != nil {
		return false, err
	}
	if shouldRotateNet {
		if err := f.a.rotateNicoMachineNetworkCandidate(ctx, nodeSet, machine); err != nil {
			return false, err
		}
		return true, nil
	}

	shouldRotateType, err := f.a.shouldRotateNicoMachineInstanceType(ctx, machine)
	if err != nil {
		return false, err
	}
	if shouldRotateType {
		if err := f.a.rotateNicoMachineInstanceType(ctx, node, machine); err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}
