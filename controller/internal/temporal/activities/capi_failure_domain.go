// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
)

// applyCAPIMachine creates a CAPI Machine or reapplies it without changing its placement.
func (a *Activities) applyCAPIMachine(ctx context.Context, machine *clusterv1.Machine) error {
	if patched, err := a.patchExistingCAPIMachine(ctx, machine); err != nil || patched {
		return err
	}
	return a.patchCAPIMachine(ctx, machine)
}

// patchExistingCAPIMachine reapplies an existing CAPI Machine while preserving its failure domain.
func (a *Activities) patchExistingCAPIMachine(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
	existing := &clusterv1.Machine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKeyFromObject(machine), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get existing CAPI machine %s: %w", machine.Name, err)
	}
	// Failure domain is selected before provisioning and persisted on the NKE
	// node. Once a CAPI Machine exists, its placement is immutable from the
	// provisioning workflow's perspective; preserve it across activity retries.
	machine.Spec.FailureDomain = existing.Spec.FailureDomain
	return true, a.patchCAPIMachine(ctx, machine)
}

// patchCAPIMachine applies the desired CAPI Machine with server-side apply.
func (a *Activities) patchCAPIMachine(ctx context.Context, machine *clusterv1.Machine) error {
	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine %s: %w", machine.Name, err)
	}
	return nil
}
