// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"cmp"
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func (a *Activities) applyCAPIMachine(ctx context.Context, createCtx *capiMachineCreateContext, machine *clusterv1.Machine) error {
	if _, isControlPlane := machine.Labels[clusterv1.MachineControlPlaneLabel]; !isControlPlane {
		return a.patchCAPIMachine(ctx, machine)
	}

	if patched, err := a.patchExistingCAPIMachine(ctx, machine); err != nil || patched {
		return err
	}

	eligibleDomains, err := a.controlPlaneFailureDomains(ctx, createCtx)
	if err != nil {
		return err
	}
	if len(eligibleDomains) == 0 {
		return a.patchCAPIMachine(ctx, machine)
	}

	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Hold the cluster row lock through selection and apply so concurrent
		// workflows observe each other's assignments.
		if _, err := tx.GetClusterForUpdate(ctx, createCtx.cluster.ID); err != nil {
			return fmt.Errorf("failed to lock cluster for failure domain placement: %w", err)
		}

		// Recheck after taking the lock in case another activity created this
		// Machine after the unlocked fast-path lookup.
		if patched, err := a.patchExistingCAPIMachine(ctx, machine); err != nil || patched {
			return err
		}

		failureDomain, err := a.selectControlPlaneFailureDomain(ctx, createCtx, eligibleDomains)
		if err != nil {
			return err
		}
		machine.Spec.FailureDomain = failureDomain
		return a.patchCAPIMachine(ctx, machine)
	})
}

func (a *Activities) patchExistingCAPIMachine(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
	existing := &clusterv1.Machine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKeyFromObject(machine), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get existing CAPI machine %s: %w", machine.Name, err)
	}
	machine.Spec.FailureDomain = existing.Spec.FailureDomain
	return true, a.patchCAPIMachine(ctx, machine)
}

func (a *Activities) controlPlaneFailureDomains(ctx context.Context, createCtx *capiMachineCreateContext) ([]string, error) {
	cluster := &clusterv1.Cluster{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Name:      createCtx.cluster.Name,
		Namespace: createCtx.namespace,
	}, cluster); err != nil {
		return nil, fmt.Errorf("failed to get CAPI cluster for failure domain placement: %w", err)
	}
	if !ptr.Deref(cluster.Status.Initialization.InfrastructureProvisioned, false) {
		return nil, fmt.Errorf("capi cluster infrastructure is not yet provisioned")
	}

	eligibleDomains := make([]string, 0, len(cluster.Status.FailureDomains))
	for _, domain := range cluster.Status.FailureDomains {
		if domain.ControlPlane != nil && *domain.ControlPlane {
			eligibleDomains = append(eligibleDomains, domain.Name)
		}
	}
	return eligibleDomains, nil
}

func (a *Activities) selectControlPlaneFailureDomain(ctx context.Context, createCtx *capiMachineCreateContext, eligibleDomains []string) (string, error) {
	machines := &clusterv1.MachineList{}
	if err := a.k8sClient.List(
		ctx,
		machines,
		client.InNamespace(createCtx.namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: createCtx.cluster.Name,
		},
	); err != nil {
		return "", fmt.Errorf("failed to list control-plane CAPI machines for failure domain placement: %w", err)
	}

	assignments := make([]string, 0, len(machines.Items))
	for _, machine := range machines.Items {
		if _, isControlPlane := machine.Labels[clusterv1.MachineControlPlaneLabel]; !isControlPlane {
			continue
		}
		// A deleting machine releases its infrastructure; its domain is free for the
		// replacement.
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}
		// Status is where the provider placed the machine; spec is only the request
		// and is used until the provider reports an observed placement.
		if domain := cmp.Or(machine.Status.FailureDomain, machine.Spec.FailureDomain); domain != "" {
			assignments = append(assignments, domain)
		}
	}

	failureDomain, err := a.failureDomainSelection(eligibleDomains, assignments)
	if err != nil {
		return "", fmt.Errorf("failed to select control-plane failure domain: %w", err)
	}
	return failureDomain, nil
}

func (a *Activities) patchCAPIMachine(ctx context.Context, machine *clusterv1.Machine) error {
	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine %s: %w", machine.Name, err)
	}
	return nil
}
