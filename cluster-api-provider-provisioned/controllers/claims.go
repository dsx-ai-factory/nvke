// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

// errHostClaimedElsewhere means another cluster holds this host. The caller
// moves on to the next candidate rather than failing: an inventory that names a
// machine someone else took is an operator mistake to report, not a reason to
// stop using the hosts that are free.
var (
	errHostClaimedElsewhere = errors.New("host is claimed by another cluster")
	errClaimRetry           = errors.New("host claim changed while it was being acquired")
)

// claimHost locks the IP address of a host for one machine.
//
// Creating the object is the lock: two claimants racing on the same address
// both call Create, and the API server rejects the second with AlreadyExists.
// That is the same compare-and-swap that makes allocation correct inside one
// cluster, extended to the only scope where a physical machine is unique.
func (r *MachineReconciler) claimHost(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	clusterName string,
	host infrav1.ProvisionedHost,
) error {
	ipAddress := host.CanonicalIPAddress()
	if ipAddress == "" {
		return fmt.Errorf("host %q has invalid IP address %q", host.Hostname, host.IPAddress)
	}
	if machine.UID == "" {
		return fmt.Errorf("ProvisionedMachine %s/%s has no UID", machine.Namespace, machine.Name)
	}
	return r.claimIPAddress(ctx, machine, clusterName, host, ipAddress)
}

func (r *MachineReconciler) claimIPAddress(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	clusterName string,
	host infrav1.ProvisionedHost,
	ipAddress string,
) error {
	claim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: infrav1.HostClaimName(ipAddress),
		},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        ipAddress,
			ClusterName:      clusterName,
			MachineNamespace: machine.Namespace,
			MachineName:      machine.Name,
			MachineUID:       string(machine.UID),
			HostName:         host.Hostname,
		},
	}

	err := r.client.Create(ctx, claim)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to claim IP address %q: %w", ipAddress, err)
	}

	existing := &infrav1.ProvisionedHostClaim{}
	if getErr := r.apiReader.Get(ctx, client.ObjectKey{Name: claim.Name}, existing); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return fmt.Errorf("%w: claim for IP address %q vanished while reading it", errClaimRetry, ipAddress)
		}
		return fmt.Errorf("failed to read the existing claim for IP address %q: %w", ipAddress, getErr)
	}

	// Already ours. Re-reconciles and retries land here, and must be a no-op.
	if existing.Spec.MachineNamespace == machine.Namespace &&
		existing.Spec.MachineName == machine.Name &&
		existing.Spec.MachineUID == string(machine.UID) {
		return nil
	}

	// Held by somebody else. A holder that no longer exists cannot be waited
	// for, so the claim is reclaimed instead: without this a provider that died
	// between claiming and binding would strand a host forever.
	holderAlive, err := r.machineExists(
		ctx,
		existing.Spec.MachineNamespace,
		existing.Spec.MachineName,
		types.UID(existing.Spec.MachineUID),
	)
	if err != nil {
		return err
	}
	if holderAlive {
		return fmt.Errorf("%w: IP address %q is held by machine %s/%s of cluster %q",
			errHostClaimedElsewhere, ipAddress,
			existing.Spec.MachineNamespace, existing.Spec.MachineName, existing.Spec.ClusterName)
	}

	if err := r.client.Delete(ctx, existing, client.Preconditions{UID: &existing.UID}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: claim for IP address %q vanished while reclaiming it", errClaimRetry, ipAddress)
		}
		return fmt.Errorf("failed to reclaim the abandoned claim for IP address %q: %w", ipAddress, err)
	}
	r.logger.Info("Reclaimed abandoned host claim",
		"claim", existing.Name,
		"ipAddress", ipAddress,
		"previousMachineNamespace", existing.Spec.MachineNamespace,
		"previousMachine", existing.Spec.MachineName,
		"previousMachineUID", existing.Spec.MachineUID,
		"claimingMachineNamespace", machine.Namespace,
		"claimingMachine", machine.Name,
		"claimingMachineUID", machine.UID,
	)
	// Retry through the requeue rather than recursing, so one reconcile can
	// never loop reclaiming a claim that something keeps recreating.
	return fmt.Errorf("%w: reclaimed the abandoned claim for IP address %q", errClaimRetry, ipAddress)
}

func (r *MachineReconciler) machineExists(ctx context.Context, namespace, name string, uid types.UID) (bool, error) {
	if namespace == "" || name == "" || uid == "" {
		return false, nil
	}
	holder := &infrav1.ProvisionedMachine{}
	err := r.apiReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, holder)
	if err == nil {
		return holder.UID == uid && holder.DeletionTimestamp.IsZero(), nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to check whether machine %s/%s still exists: %w", namespace, name, err)
}

// releaseClaims drops every claim held by a machine.
//
// The machine UID prevents cleanup of a new machine that reuses the same
// namespace and name.
func (r *MachineReconciler) releaseClaims(ctx context.Context, machine *infrav1.ProvisionedMachine) error {
	claims := &infrav1.ProvisionedHostClaimList{}
	if err := r.apiReader.List(ctx, claims); err != nil {
		return fmt.Errorf("failed to list the claims of machine %s/%s: %w", machine.Namespace, machine.Name, err)
	}

	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.MachineNamespace != machine.Namespace ||
			claim.Spec.MachineName != machine.Name ||
			claim.Spec.MachineUID != string(machine.UID) {
			continue
		}
		if err := r.client.Delete(ctx, claim, client.Preconditions{UID: &claim.UID}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to release claim %s: %w", claim.Name, err)
		}
		r.logger.Info("Released host claim",
			"claim", claim.Name,
			"ipAddress", claim.Spec.IPAddress,
			"machineNamespace", machine.Namespace,
			"machine", machine.Name,
			"machineUID", machine.UID,
		)
	}
	return nil
}
