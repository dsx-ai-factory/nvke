// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// applyCAPIMachine creates a CAPI Machine, or reapplies one that already exists
// while guarding against any change to the spec's failure domain.
func (a *Activities) applyCAPIMachine(ctx context.Context, machine *clusterv1.Machine) error {
	existing := &clusterv1.Machine{}
	switch err := a.k8sClient.Get(ctx, client.ObjectKeyFromObject(machine), existing); {
	case apierrors.IsNotFound(err):
		// No Machine yet; the apply below creates it with the placement this
		// attempt computed.
	case err != nil:
		return fmt.Errorf("failed to get existing CAPI machine %s: %w", machine.Name, err)
	default:
		machine.Spec.FailureDomain = existing.Spec.FailureDomain
	}

	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(a.cfg.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine %s: %w", machine.Name, err)
	}
	return nil
}
