// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

var (
	// errHostClaimedElsewhere means another cluster holds this host.
	errHostClaimedElsewhere = errors.New("host is claimed by another cluster")

	// errHostClaimedByStaleMachine means this cluster has a permanent claim from
	// a previous incarnation of a machine. An operator must verify the host is
	// safe before deleting that claim manually.
	errHostClaimedByStaleMachine = errors.New("host is claimed by a previous machine of this cluster")
)

// machineHasClaim reports whether a permanent host claim belongs to the
// current incarnation of a machine. It is used only during deletion, to avoid
// treating a machine that stopped between claim creation and provider-ID
// persistence as unbound.
func (r *MachineReconciler) machineHasClaim(ctx context.Context, machine *infrav1.ProvisionedMachine) (bool, error) {
	claims := &infrav1.ProvisionedHostClaimList{}
	if err := r.apiReader.List(ctx, claims); err != nil {
		return false, fmt.Errorf("failed to list host claims while deleting ProvisionedMachine %s/%s: %w",
			machine.Namespace, machine.Name, err)
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.MachineNamespace == machine.Namespace &&
			claim.Spec.MachineName == machine.Name &&
			claim.Spec.MachineUID == string(machine.UID) {
			return true, nil
		}
	}
	return false, nil
}

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
) (ctrl.Result, error) {
	ipAddress := host.CanonicalIPAddress()
	if ipAddress == "" {
		return ctrl.Result{}, fmt.Errorf("host %q has invalid IP address %q", host.Hostname, host.IPAddress)
	}
	if machine.UID == "" {
		return ctrl.Result{}, fmt.Errorf("ProvisionedMachine %s/%s has no UID", machine.Namespace, machine.Name)
	}
	return r.claimIPAddress(ctx, machine, clusterName, host, ipAddress)
}

func (r *MachineReconciler) claimIPAddress(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	clusterName string,
	host infrav1.ProvisionedHost,
	ipAddress string,
) (ctrl.Result, error) {
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
		return ctrl.Result{}, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("failed to claim IP address %q: %w", ipAddress, err)
	}

	existing := &infrav1.ProvisionedHostClaim{}
	if getErr := r.apiReader.Get(ctx, client.ObjectKey{Name: claim.Name}, existing); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to read the existing claim for IP address %q: %w", ipAddress, getErr)
	}

	// Already ours. Re-reconciles and retries land here, and must be a no-op.
	if existing.Spec.MachineNamespace == machine.Namespace &&
		existing.Spec.MachineName == machine.Name &&
		existing.Spec.MachineUID == string(machine.UID) {
		return ctrl.Result{}, nil
	}
	if existing.Spec.ClusterName == clusterName {
		return ctrl.Result{}, fmt.Errorf(
			"%w: claim %q for IP address %q belongs to machine %s/%s with UID %q; verify the host is safe before deleting the claim",
			errHostClaimedByStaleMachine, existing.Name, ipAddress,
			existing.Spec.MachineNamespace, existing.Spec.MachineName, existing.Spec.MachineUID,
		)
	}

	// Claims are permanent for the life of this bootstrap management cluster.
	// The provider does not support deleting individual machines, so it must
	// not infer that a host is safe to reuse from the absence of its holder.
	return ctrl.Result{}, fmt.Errorf("%w: IP address %q is held by machine %s/%s of cluster %q",
		errHostClaimedElsewhere, ipAddress,
		existing.Spec.MachineNamespace, existing.Spec.MachineName, existing.Spec.ClusterName)
}
