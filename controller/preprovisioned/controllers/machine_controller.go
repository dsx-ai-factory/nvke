// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/NVIDIA/nke/controller/preprovisioned/api/v1alpha1"
)

const (
	// retryAfter is the wait before the provider looks again for a free host.
	// The set of hosts only changes when an operator edits the inventory or when
	// a machine is deleted, so a short retry would only add load.
	retryAfter = 30 * time.Second

	preprovisionedClusterKind = "PreprovisionedCluster"
)

// MachineReconciler binds a PreprovisionedMachine to one host of the inventory.
//
// The reconciler creates no infrastructure. The host is already on, and the
// network already gave it an address. The reconciler records which machine
// holds which host, and it publishes the address of that host so that Cluster
// API and CWE can reach the node.
type MachineReconciler struct {
	client client.Client
	logger *slog.Logger
}

// NewMachineReconciler builds a MachineReconciler.
func NewMachineReconciler(k8sClient client.Client, logger *slog.Logger) *MachineReconciler {
	return &MachineReconciler{
		client: k8sClient,
		logger: logger,
	}
}

// SetupWithManager registers the reconciler with a manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.PreprovisionedMachine{}).
		Named("preprovisioned-machine").
		Complete(r)
}

// Reconcile gives the machine a host, or reports why it cannot.
//
// The provider holds no finalizer. It creates nothing, so a deleted machine
// needs no cleanup: the host becomes free again as soon as the object is gone,
// because the set of used hosts is derived from the objects that exist.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.logger.With("namespace", req.Namespace, "preprovisionedMachine", req.Name)

	machine := &infrav1.PreprovisionedMachine{}
	if err := r.client.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			// The machine is gone, so its hosts are free. Releasing here rather
			// than from a finalizer keeps a wedged provider from blocking a
			// delete; a release missed while the provider was down is recovered
			// by the next claimant, which reclaims a claim whose holder no
			// longer exists.
			return ctrl.Result{}, r.releaseClaims(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PreprovisionedMachine: %w", err)
	}

	if !machine.DeletionTimestamp.IsZero() {
		logger.Debug("PreprovisionedMachine is being deleted, skipping")
		return ctrl.Result{}, nil
	}

	ownerMachine, err := capiutil.GetOwnerMachine(ctx, r.client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner Machine: %w", err)
	}
	if ownerMachine == nil {
		// Cluster API sets the owner reference shortly after CWE applies the
		// Machine. Until then the role of this machine is unknown, so it cannot
		// take a host that a control plane machine may need.
		logger.Info("Waiting for Machine controller to set the owner reference")
		return ctrl.Result{}, r.setNotReady(ctx, machine, infrav1.WaitingForOwnerReason, "Waiting for the owner Machine")
	}

	infraCluster, found, err := r.infraClusterFor(ctx, machine, ownerMachine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		logger.Info("Waiting for the PreprovisionedCluster")
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.WaitingForClusterReason, "Waiting for the PreprovisionedCluster")
	}

	// An allocation that already exists is never revisited. The binding is
	// permanent for the life of the machine: a node runs on that host.
	if machine.Spec.ProviderID != "" {
		return ctrl.Result{}, r.reconcileBoundMachine(ctx, machine, infraCluster, logger)
	}

	return r.allocateHost(ctx, machine, infraCluster, capiutil.IsControlPlaneMachine(ownerMachine), logger)
}

// infraClusterFor finds the PreprovisionedCluster of a machine through the
// Cluster that owns it. The bool reports whether the object exists yet; while
// it is false the caller waits rather than fails.
func (r *MachineReconciler) infraClusterFor(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	ownerMachine *clusterv1.Machine,
) (*infrav1.PreprovisionedCluster, bool, error) {
	if ownerMachine.Spec.ClusterName == "" {
		return nil, false, fmt.Errorf(
			"machine %s/%s has an empty spec.clusterName",
			ownerMachine.Namespace, ownerMachine.Name,
		)
	}

	cluster := &clusterv1.Cluster{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: ownerMachine.Namespace,
		Name:      ownerMachine.Spec.ClusterName,
	}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get Cluster %s: %w", ownerMachine.Spec.ClusterName, err)
	}

	// Read the name from the Cluster rather than assume it matches the cluster
	// name. Cluster API treats the infrastructure reference as the only link.
	infraRef := cluster.Spec.InfrastructureRef
	if infraRef.Name == "" {
		return nil, false, nil
	}
	if infraRef.Kind != preprovisionedClusterKind || infraRef.APIGroup != infrav1.GroupName {
		return nil, false, fmt.Errorf(
			"cluster %s/%s has infrastructure %s.%s, not %s.%s",
			cluster.Namespace, cluster.Name, infraRef.Kind, infraRef.APIGroup,
			preprovisionedClusterKind, infrav1.GroupName,
		)
	}

	infraCluster := &infrav1.PreprovisionedCluster{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      infraRef.Name,
	}, infraCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get PreprovisionedCluster %s: %w", infraRef.Name, err)
	}

	return infraCluster, true, nil
}

// allocateHost selects a free host and writes the binding.
//
// The write uses optimistic concurrency. If two machines select the same host
// at the same time, the API server accepts only the first write. The loser gets
// a conflict, and the requeue repeats the search against a fresh view, so no
// host is ever given to two machines.
func (r *MachineReconciler) allocateHost(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	infraCluster *infrav1.PreprovisionedCluster,
	controlPlane bool,
	logger *slog.Logger,
) (ctrl.Result, error) {
	// A machine that holds no host must hold no claims. Stating it as an
	// invariant costs one list and makes the claim set self-healing: a provider
	// that died between claiming and binding leaves claims that this releases
	// before it selects again, so no host is stranded by a crash.
	if err := r.releaseClaims(ctx, machine.Namespace, machine.Name); err != nil {
		return ctrl.Result{}, err
	}

	machines, err := r.listClusterMachines(ctx, machine.Namespace, infraCluster.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	allocated := allocationsFromMachines(infraCluster.Name, machines)

	candidates := selectHosts(infraCluster.Spec.Hosts, allocated, controlPlane, machine.Spec.HostSelector)
	if len(candidates) == 0 {
		logger.Info("No free host in the inventory",
			"controlPlane", controlPlane,
			"hosts", len(infraCluster.Spec.Hosts),
			"allocated", len(allocated),
		)
		message := fmt.Sprintf(
			"No free host for this machine: the inventory has %d hosts and %d are in use",
			len(infraCluster.Spec.Hosts), len(allocated),
		)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.NoHostAvailableReason, message)
	}

	// Walk the candidates rather than commit to the first. A host another
	// cluster already locked is not this machine's to use, but the ones after
	// it may well be, so an overlapping inventory costs a host and not the
	// whole node.
	var host infrav1.PreprovisionedHost
	var claimed bool
	var lastConflict error
	for _, candidate := range candidates {
		err := r.claimHost(ctx, machine, infraCluster.Name, candidate)
		if err == nil {
			host = candidate
			claimed = true
			break
		}
		if errors.Is(err, errHostClaimedElsewhere) {
			logger.Info("Host is locked by another cluster, trying the next one",
				"host", candidate.Name, "reason", err.Error())
			lastConflict = err
			continue
		}
		return ctrl.Result{}, err
	}
	if !claimed {
		message := fmt.Sprintf(
			"Every free host is locked by another cluster, so the inventories overlap: %v", lastConflict)
		logger.Error("No host could be locked", "error", lastConflict)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.HostsClaimedElsewhereReason, message)
	}

	machine.Spec.ProviderID = infrav1.FormatProviderID(infraCluster.Name, host.Name)
	if err := r.client.Update(ctx, machine); err != nil {
		if apierrors.IsConflict(err) {
			// Another write reached the object first. Look again with a fresh
			// view rather than force this selection, and drop the locks taken
			// for a binding that did not happen.
			logger.Info("Conflict while claiming a host, retrying", "host", host.Name)
			if releaseErr := r.releaseClaims(ctx, machine.Namespace, machine.Name); releaseErr != nil {
				return ctrl.Result{}, releaseErr
			}
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to claim host %s: %w", host.Name, err)
	}

	logger.Info("Bound machine to host", "host", host.Name, "providerID", machine.Spec.ProviderID)

	// The status write follows the spec write. If the provider stops between
	// the two, the next reconcile sees the provider ID and repairs the status.
	return ctrl.Result{}, r.applyHostStatus(ctx, machine, host)
}

// reconcileBoundMachine refreshes the status of a machine that already holds a
// host.
func (r *MachineReconciler) reconcileBoundMachine(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	infraCluster *infrav1.PreprovisionedCluster,
	logger *slog.Logger,
) error {
	clusterName, hostName, err := infrav1.ParseProviderID(machine.Spec.ProviderID)
	if err != nil {
		return fmt.Errorf("PreprovisionedMachine %s has an unreadable provider ID: %w", machine.Name, err)
	}
	if clusterName != infraCluster.Name {
		return fmt.Errorf(
			"PreprovisionedMachine %s holds host %q of cluster %q, but it belongs to cluster %q",
			machine.Name, hostName, clusterName, infraCluster.Name,
		)
	}

	host, found := infraCluster.Host(hostName)
	if !found {
		// The operator removed the host from the inventory while a node runs on
		// it. Keep the binding and report the problem: releasing the host would
		// give a running node to a second machine.
		logger.Warn("Host of a bound machine is no longer in the inventory", "host", hostName)
		message := fmt.Sprintf("Host %q is no longer in the inventory of the cluster", hostName)
		return r.setNotReady(ctx, machine, infrav1.HostNotFoundReason, message)
	}

	return r.applyHostStatus(ctx, machine, host)
}

// applyHostStatus publishes the address of the host and reports the machine as
// provisioned. Nothing is created, so a machine that holds a host is ready.
func (r *MachineReconciler) applyHostStatus(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	host infrav1.PreprovisionedHost,
) error {
	patch := client.MergeFrom(machine.DeepCopy())

	machine.Status.HostName = host.Name
	machine.Status.Addresses = append([]clusterv1.MachineAddress(nil), host.Addresses...)
	machine.Status.Initialization.Provisioned = ptr.To(true)
	setCondition(machine, metav1.ConditionTrue, infrav1.HostAllocatedReason,
		fmt.Sprintf("Machine holds host %q", host.Name))

	if err := r.client.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("failed to update the status of PreprovisionedMachine %s: %w", machine.Name, err)
	}
	return nil
}

// setNotReady reports why the machine holds no host. It never clears a binding
// or the provisioned flag: both describe a host that a node may already use.
func (r *MachineReconciler) setNotReady(
	ctx context.Context,
	machine *infrav1.PreprovisionedMachine,
	reason, message string,
) error {
	patch := client.MergeFrom(machine.DeepCopy())
	setCondition(machine, metav1.ConditionFalse, reason, message)
	if err := r.client.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("failed to update the status of PreprovisionedMachine %s: %w", machine.Name, err)
	}
	return nil
}

// listClusterMachines returns every PreprovisionedMachine of one cluster in one
// namespace. CWE puts each cluster in its own namespace, and Cluster API labels
// every rendered machine with the cluster name, so the label narrows the list
// even if a namespace ever holds two clusters.
func (r *MachineReconciler) listClusterMachines(
	ctx context.Context,
	namespace, clusterName string,
) ([]infrav1.PreprovisionedMachine, error) {
	machines := &infrav1.PreprovisionedMachineList{}
	if err := r.client.List(ctx, machines,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
	); err != nil {
		return nil, fmt.Errorf("failed to list PreprovisionedMachines: %w", err)
	}
	return machines.Items, nil
}

// conditionSetter is met by both objects of this provider.
type conditionSetter interface {
	client.Object
	GetConditions() []metav1.Condition
	SetConditions(conditions []metav1.Condition)
}

// setCondition writes the Ready condition that the Cluster API contract reads.
func setCondition(obj conditionSetter, status metav1.ConditionStatus, reason, message string) {
	conditions := obj.GetConditions()
	condition := metav1.Condition{
		Type:               infrav1.ReadyCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	}

	for i := range conditions {
		if conditions[i].Type != infrav1.ReadyCondition {
			continue
		}
		// Keep the original transition time while the status is unchanged, so
		// the age of a condition stays meaningful.
		condition.LastTransitionTime = conditions[i].LastTransitionTime
		if conditions[i].Status != status {
			condition.LastTransitionTime = metav1.Now()
		}
		conditions[i] = condition
		obj.SetConditions(conditions)
		return
	}

	condition.LastTransitionTime = metav1.Now()
	obj.SetConditions(append(conditions, condition))
}
