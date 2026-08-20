// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/NVIDIA/nke/controller/preprovisioned/api/v1alpha1"
)

// errHostClaimedElsewhere means another cluster holds this host. The caller
// moves on to the next candidate rather than failing: an inventory that names a
// machine someone else took is an operator mistake to report, not a reason to
// stop using the hosts that are free.
var errHostClaimedElsewhere = errors.New("host is claimed by another cluster")

// claimHost locks every identity of a host for one machine.
//
// Creating the object is the lock: two claimants racing on the same identity
// both call Create, and the API server rejects the second with AlreadyExists.
// That is the same compare-and-swap that makes allocation correct inside one
// cluster, extended to the only scope where a physical machine is unique.
//
// The claim is all-or-nothing. A host with a machineID and two addresses is
// three locks, and holding two of them would leave the host half-locked and
// still available to another cluster through the third, so a partial failure
// releases what this pass took.
func (r *MachineReconciler) claimHost(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	clusterName string,
	host infrav1.PreprovisionedHost,
) error {
	acquired := make([]string, 0, len(host.Addresses)+1)

	for _, identity := range host.Identities() {
		if err := r.claimIdentity(ctx, machine, clusterName, host, identity); err != nil {
			// Undo this pass. Anything already held by this machine from an
			// earlier pass is released by releaseClaims before the next attempt.
			r.releaseIdentities(ctx, acquired)
			return err
		}
		acquired = append(acquired, identity)
	}

	return nil
}

func (r *MachineReconciler) claimIdentity(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	clusterName string,
	host infrav1.PreprovisionedHost,
	identity string,
) error {
	claim := &infrav1.PreprovisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: infrav1.HostClaimName(identity),
			Labels: map[string]string{
				infrav1.ClaimClusterLabel:          clusterName,
				infrav1.ClaimMachineNamespaceLabel: machine.Namespace,
				infrav1.ClaimMachineLabel:          machine.Name,
			},
		},
		Spec: infrav1.PreprovisionedHostClaimSpec{
			Identity:         identity,
			ClusterName:      clusterName,
			MachineNamespace: machine.Namespace,
			MachineName:      machine.Name,
			HostName:         host.Name,
		},
	}

	err := r.client.Create(ctx, claim)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to claim identity %q: %w", identity, err)
	}

	existing := &infrav1.PreprovisionedHostClaim{}
	if getErr := r.client.Get(ctx, client.ObjectKey{Name: claim.Name}, existing); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			// Released between the create and the read. Say so rather than
			// guess; the requeue retries against a settled view.
			return fmt.Errorf("claim for identity %q vanished while reading it", identity)
		}
		return fmt.Errorf("failed to read the existing claim for identity %q: %w", identity, getErr)
	}

	// Already ours. Re-reconciles and retries land here, and must be a no-op.
	if existing.Spec.MachineNamespace == machine.Namespace && existing.Spec.MachineName == machine.Name {
		return nil
	}

	// Held by somebody else. A holder that no longer exists cannot be waited
	// for, so the claim is reclaimed instead: without this a provider that died
	// between claiming and binding would strand a host forever.
	holderAlive, err := r.machineExists(ctx, existing.Spec.MachineNamespace, existing.Spec.MachineName)
	if err != nil {
		return err
	}
	if holderAlive {
		return fmt.Errorf("%w: identity %q is held by machine %s/%s of cluster %q",
			errHostClaimedElsewhere, identity,
			existing.Spec.MachineNamespace, existing.Spec.MachineName, existing.Spec.ClusterName)
	}

	if err := r.client.Delete(ctx, existing, client.Preconditions{UID: &existing.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to reclaim the abandoned claim for identity %q: %w", identity, err)
	}
	// Retry through the requeue rather than recursing, so one reconcile can
	// never loop reclaiming a claim that something keeps recreating.
	return fmt.Errorf("reclaimed the abandoned claim for identity %q, retrying", identity)
}

func (r *MachineReconciler) machineExists(ctx context.Context, namespace, name string) (bool, error) {
	if namespace == "" || name == "" {
		return false, nil
	}
	holder := &infrav1.PreprovisionedMachine{}
	err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, holder)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to check whether machine %s/%s still exists: %w", namespace, name, err)
}

// releaseClaims drops every claim held by a machine.
//
// It runs on two paths: a machine that holds no host must hold no claims, and a
// machine that is gone must leave none behind. Both are stated as invariants
// rather than tracked, so the provider needs no finalizer — a wedged provider
// can never block a machine from being deleted, which matters more on hardware
// that CWE does not own.
func (r *MachineReconciler) releaseClaims(ctx context.Context, namespace, name string) error {
	claims := &infrav1.PreprovisionedHostClaimList{}
	if err := r.client.List(ctx, claims, client.MatchingLabels{
		infrav1.ClaimMachineNamespaceLabel: namespace,
		infrav1.ClaimMachineLabel:          name,
	}); err != nil {
		return fmt.Errorf("failed to list the claims of machine %s/%s: %w", namespace, name, err)
	}

	for i := range claims.Items {
		claim := &claims.Items[i]
		if err := r.client.Delete(ctx, claim, client.Preconditions{UID: &claim.UID}); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to release claim %s: %w", claim.Name, err)
		}
	}
	return nil
}

// releaseIdentities drops the claims taken in one pass. Best effort: the caller
// is already failing, and anything left behind is released by the next
// reconcile's releaseClaims or reclaimed once the machine is gone.
func (r *MachineReconciler) releaseIdentities(ctx context.Context, identities []string) {
	for _, identity := range identities {
		claim := &infrav1.PreprovisionedHostClaim{ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(identity)}}
		_ = r.client.Delete(ctx, claim)
	}
}
