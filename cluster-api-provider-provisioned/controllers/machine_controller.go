// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedmachines,verbs=get;list;patch;update;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedmachines/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedhostclaims,verbs=create;get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

const (
	// retryAfter is the wait before the provider looks again for a free host.
	// The set of hosts only changes when an operator edits the inventory or a
	// machine acquires a host, so a short retry would only add load.
	retryAfter = 30 * time.Second

	provisionedClusterKind = "ProvisionedCluster"
	machineFinalizer       = "provisionedmachine.infrastructure.cluster.x-k8s.io"
)

// MachineReconciler binds a ProvisionedMachine to one host of the inventory.
//
// The reconciler creates no infrastructure. The host is already on, and the
// network already gave it an address. The reconciler records which machine
// holds which host, and it publishes the address of that host so that Cluster
// API and CWE can reach the node.
type MachineReconciler struct {
	client        client.Client
	apiReader     client.Reader
	tenantClients tenantClientFactory
	logger        *slog.Logger
}

// NewMachineReconciler builds a MachineReconciler.
func NewMachineReconciler(k8sClient client.Client, apiReader client.Reader, logger *slog.Logger) *MachineReconciler {
	return &MachineReconciler{
		client:        k8sClient,
		apiReader:     apiReader,
		tenantClients: newKubeconfigTenantClientFactory(apiReader, k8sClient.Scheme()),
		logger:        logger,
	}
}

// SetupWithManager registers the reconciler with a manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ProvisionedMachine{}).
		Named("provisioned-machine").
		Complete(r)
}

// Reconcile gives the machine a host, or reports why it cannot.
//
// A finalizer blocks deletion after a machine claims a host. This
// bootstrap-only provider cannot reset or power off the host, so it cannot make
// that host safe for reuse. A machine with no provider ID and no claim can be
// deleted normally.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.logger.With("namespace", req.Namespace, "provisionedMachine", req.Name)

	machine := &infrav1.ProvisionedMachine{}
	if err := r.client.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ProvisionedMachine: %w", err)
	}

	if !machine.DeletionTimestamp.IsZero() {
		if machine.Spec.ProviderID == "" {
			hasClaim, err := r.machineHasClaim(ctx, machine)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !hasClaim {
				patch := client.MergeFrom(machine.DeepCopy())
				controllerutil.RemoveFinalizer(machine, machineFinalizer)
				if err := r.client.Patch(ctx, machine, patch); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to remove the finalizer from an unbound ProvisionedMachine: %w", err)
				}
				logger.Info("Allowed deletion of an unbound ProvisionedMachine")
				return ctrl.Result{}, nil
			}
		}

		// TODO(jonathanmeier5): Support deleting a ProvisionedMachine after the
		// provider has a safe host teardown and reuse lifecycle.
		message := "Individual ProvisionedMachine deletion is not supported; destroy the bootstrap management cluster"
		logger.Warn("ProvisionedMachine deletion is not supported")
		return ctrl.Result{},
			r.setNotReady(ctx, machine, infrav1.DeletionNotSupportedReason, message)
	}

	if !controllerutil.ContainsFinalizer(machine, machineFinalizer) {
		patch := client.MergeFrom(machine.DeepCopy())
		controllerutil.AddFinalizer(machine, machineFinalizer)
		if err := r.client.Patch(ctx, machine, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add the ProvisionedMachine finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	ownerMachine, err := capiutil.GetOwnerMachine(ctx, r.client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner Machine: %w", err)
	}
	if ownerMachine == nil {
		// Cluster API sets the owner reference shortly after CWE applies the
		// Machine. Until then the owning cluster is unknown, so the provider
		// cannot select the correct inventory.
		logger.Info("Waiting for Machine controller to set the owner reference")
		return ctrl.Result{}, r.setNotReady(ctx, machine, infrav1.WaitingForOwnerReason, "Waiting for the owner Machine")
	}

	infraCluster, found, err := r.infraClusterFor(ctx, machine, ownerMachine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		logger.Info("Waiting for the ProvisionedCluster")
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.WaitingForClusterReason, "Waiting for the ProvisionedCluster")
	}

	// An allocation that already exists is never revisited. The binding is
	// permanent for the life of the machine: a node runs on that host.
	if machine.Spec.ProviderID != "" {
		result, err := r.reconcileBoundMachine(ctx, machine, infraCluster, logger)
		if err != nil || !result.IsZero() {
			return result, err
		}
		return r.reconcileNodeProviderID(ctx, machine, ownerMachine, logger)
	}

	if err := validateInventory(infraCluster.Spec.Hosts); err != nil {
		logger.Error("Cannot allocate a host from an invalid inventory", "error", err)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.InventoryInvalidReason, err.Error())
	}

	result, err := r.allocateHost(ctx, machine, infraCluster, logger)
	if err != nil || result != (ctrl.Result{}) {
		return result, err
	}
	return r.reconcileNodeProviderID(ctx, machine, ownerMachine, logger)
}

// infraClusterFor finds the ProvisionedCluster of a machine through the
// Cluster that owns it. The bool reports whether the object exists yet; while
// it is false the caller waits rather than fails.
func (r *MachineReconciler) infraClusterFor(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	ownerMachine *clusterv1.Machine,
) (*infrav1.ProvisionedCluster, bool, error) {
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
	if infraRef.Kind != provisionedClusterKind || infraRef.APIGroup != infrav1.GroupName {
		return nil, false, fmt.Errorf(
			"cluster %s/%s has infrastructure %s.%s, not %s.%s",
			cluster.Namespace, cluster.Name, infraRef.Kind, infraRef.APIGroup,
			provisionedClusterKind, infrav1.GroupName,
		)
	}

	infraCluster := &infrav1.ProvisionedCluster{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      infraRef.Name,
	}, infraCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get ProvisionedCluster %s: %w", infraRef.Name, err)
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
	machine *infrav1.ProvisionedMachine,
	infraCluster *infrav1.ProvisionedCluster,
	logger *slog.Logger,
) (ctrl.Result, error) {
	machines, err := r.listClusterMachines(ctx, machine.Namespace, infraCluster.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	allocated := allocationsFromMachines(infraCluster.Name, machines)

	initNodeClaimed := false
	for _, host := range infraCluster.Spec.Hosts {
		if host.Hostname != infraCluster.Spec.InitNode {
			continue
		}
		initClaim := &infrav1.ProvisionedHostClaim{}
		err = r.apiReader.Get(ctx, client.ObjectKey{Name: infrav1.HostClaimName(host.CanonicalIPAddress())}, initClaim)
		initNodeClaimed = err == nil
		if initNodeClaimed &&
			initClaim.Spec.MachineNamespace == machine.Namespace &&
			initClaim.Spec.MachineName == machine.Name &&
			initClaim.Spec.MachineUID == string(machine.UID) {
			// A restart can occur after the claim is created and before the
			// provider ID is written. Keep the holder on the init node.
			initNodeClaimed = false
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to read the init-node claim: %w", err)
		}
		break
	}

	candidates := selectHosts(
		infraCluster.Spec.Hosts,
		allocated,
		machine.Spec.HostSelector,
		infraCluster.Spec.InitNode,
		initNodeClaimed,
	)
	if len(candidates) == 0 {
		logger.Info("No free host in the inventory",
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
	var host infrav1.ProvisionedHost
	var claimed bool
	var lastConflict error
	var staleConflict error
	for _, candidate := range candidates {
		claimResult, err := r.claimHost(ctx, machine, infraCluster.Name, candidate)
		if errors.Is(err, errHostClaimedElsewhere) || errors.Is(err, errHostClaimedByStaleMachine) {
			logger.Info("Host has a permanent claim, trying the next one",
				"host", candidate.Hostname, "reason", err.Error())
			lastConflict = err
			if errors.Is(err, errHostClaimedByStaleMachine) {
				staleConflict = err
			}
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if !claimResult.IsZero() {
			logger.Info("Host claim changed while it was being acquired, retrying",
				"host", candidate.Hostname,
			)
			return claimResult, nil
		}
		host = candidate
		claimed = true
		break
	}
	if !claimed {
		reason := infrav1.HostsClaimedElsewhereReason
		conflict := lastConflict
		message := fmt.Sprintf(
			"Every free host is locked by another cluster, so the inventories overlap: %v", conflict)
		if staleConflict != nil {
			reason = infrav1.StaleHostClaimReason
			conflict = staleConflict
			message = fmt.Sprintf("A permanent host claim belongs to a previous machine of this cluster: %v", conflict)
		}
		logger.Error("No host could be locked", "error", conflict)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, reason, message)
	}

	machine.Spec.ProviderID = infrav1.FormatProviderID(infraCluster.Name, host.IPAddress)
	if err := r.client.Update(ctx, machine); err != nil {
		if apierrors.IsConflict(err) {
			// Another write reached the object first. Look again with a fresh
			// view rather than force this selection. The claim is already owned
			// by this machine, so the next reconcile can reuse it.
			logger.Info("Conflict while claiming a host, retrying", "host", host.Hostname)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to claim host %s: %w", host.Hostname, err)
	}

	logger.Info("Bound machine to host", "host", host.Hostname, "providerID", machine.Spec.ProviderID)

	// The status write follows the spec write. If the provider stops between
	// the two, the next reconcile sees the provider ID and repairs the status.
	return ctrl.Result{}, r.applyHostStatus(ctx, machine, host)
}

// reconcileBoundMachine refreshes the status of a machine that already holds a
// host.
func (r *MachineReconciler) reconcileBoundMachine(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	infraCluster *infrav1.ProvisionedCluster,
	logger *slog.Logger,
) (ctrl.Result, error) {
	clusterName, ipAddress, err := infrav1.ParseProviderID(machine.Spec.ProviderID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ProvisionedMachine %s has an unreadable provider ID: %w", machine.Name, err)
	}
	if clusterName != infraCluster.Name {
		return ctrl.Result{}, fmt.Errorf(
			"ProvisionedMachine %s holds host %q of cluster %q, but it belongs to cluster %q",
			machine.Name, ipAddress, clusterName, infraCluster.Name,
		)
	}

	host, found := infraCluster.HostByIPAddress(ipAddress)
	if !found {
		// The operator removed the host from the inventory while a node runs on
		// it. Keep the binding and report the problem: releasing the host would
		// give a running node to a second machine.
		logger.Warn("Host of a bound machine is no longer in the inventory", "ipAddress", ipAddress)
		message := fmt.Sprintf("Host with IP address %q is no longer in the inventory of the cluster", ipAddress)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.HostNotFoundReason, message)
	}

	claimResult, err := r.claimHost(ctx, machine, infraCluster.Name, host)
	if errors.Is(err, errHostClaimedByStaleMachine) {
		message := fmt.Sprintf("Host %q has a permanent claim from a previous machine of this cluster: %v", host.Hostname, err)
		logger.Error("Host of a bound machine has a stale claim", "host", host.Hostname, "error", err)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.StaleHostClaimReason, message)
	}
	if errors.Is(err, errHostClaimedElsewhere) {
		message := fmt.Sprintf("Host %q cannot be used because its IP address is claimed by another machine: %v", host.Hostname, err)
		logger.Error("Host of a bound machine is claimed elsewhere", "host", host.Hostname, "error", err)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setNotReady(ctx, machine, infrav1.HostsClaimedElsewhereReason, message)
	}
	if err != nil || !claimResult.IsZero() {
		return claimResult, err
	}

	return ctrl.Result{}, r.applyHostStatus(ctx, machine, host)
}

// reconcileNodeProviderID copies the infrastructure identity to the tenant
// Node after the host is allocated. The tenant Node can appear after the
// ProvisionedMachine becomes ready, so missing prerequisites cause a timed
// requeue.
func (r *MachineReconciler) reconcileNodeProviderID(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	ownerMachine *clusterv1.Machine,
	logger *slog.Logger,
) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	clusterKey := client.ObjectKey{Namespace: ownerMachine.Namespace, Name: ownerMachine.Spec.ClusterName}
	if err := r.client.Get(ctx, clusterKey, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Waiting for the CAPI Cluster", "cluster", ownerMachine.Spec.ClusterName)
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Cluster for tenant Node: %w", err)
	}

	tenantCtx, cancel := context.WithTimeout(ctx, tenantAPITimeout)
	defer cancel()

	tenantClient, err := r.tenantClients.ClientForCluster(tenantCtx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Waiting for the tenant kubeconfig Secret", "cluster", cluster.Name)
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to create tenant client: %w", err)
	}

	node := &corev1.Node{}
	if err := tenantClient.Get(tenantCtx, client.ObjectKey{Name: ownerMachine.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Waiting for the tenant Node", "node", ownerMachine.Name)
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get tenant Node: %w", err)
	}
	if node.Spec.ProviderID == machine.Spec.ProviderID {
		return ctrl.Result{}, nil
	}
	if node.Spec.ProviderID != "" {
		message := fmt.Sprintf("Tenant Node %s has provider ID %q, expected %q",
			node.Name, node.Spec.ProviderID, machine.Spec.ProviderID)
		logger.Error("Tenant Node holds a different provider ID", "node", node.Name,
			"providerID", node.Spec.ProviderID, "expectedProviderID", machine.Spec.ProviderID)
		return ctrl.Result{}, r.setNotReady(ctx, machine, infrav1.NodeProviderIDConflictReason, message)
	}

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Spec.ProviderID = machine.Spec.ProviderID
	if err := tenantClient.Patch(tenantCtx, node, patch); err != nil {
		if apierrors.IsConflict(err) {
			logger.Info("Tenant Node changed while setting its provider ID; retrying", "node", node.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to patch tenant Node provider ID: %w", err)
	}

	logger.Info("Set tenant Node provider ID", "node", node.Name, "providerID", node.Spec.ProviderID)
	return ctrl.Result{}, nil
}

// applyHostStatus publishes the address of the host and reports the machine as
// provisioned. Nothing is created, so a machine that holds a host is ready.
func (r *MachineReconciler) applyHostStatus(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	host infrav1.ProvisionedHost,
) error {
	patch := client.MergeFrom(machine.DeepCopy())

	machine.Status.HostName = host.Hostname
	machine.Status.Addresses = machineAddresses(host)
	machine.Status.Initialization.Provisioned = ptr.To(true)
	setCondition(machine, metav1.ConditionTrue, infrav1.HostAllocatedReason,
		fmt.Sprintf("Machine holds host %q", host.Hostname))

	if err := r.client.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("failed to update the status of ProvisionedMachine %s: %w", machine.Name, err)
	}
	return nil
}

// setNotReady reports why the machine holds no host. It never clears a binding
// or the provisioned flag: both describe a host that a node may already use.
func (r *MachineReconciler) setNotReady(
	ctx context.Context,
	machine *infrav1.ProvisionedMachine,
	reason, message string,
) error {
	patch := client.MergeFrom(machine.DeepCopy())
	setCondition(machine, metav1.ConditionFalse, reason, message)
	if err := r.client.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("failed to update the status of ProvisionedMachine %s: %w", machine.Name, err)
	}
	return nil
}

// listClusterMachines returns every ProvisionedMachine of one cluster in one
// namespace. CWE puts each cluster in its own namespace, and Cluster API labels
// every rendered machine with the cluster name, so the label narrows the list
// even if a namespace ever holds two clusters.
func (r *MachineReconciler) listClusterMachines(
	ctx context.Context,
	namespace, clusterName string,
) ([]infrav1.ProvisionedMachine, error) {
	machines := &infrav1.ProvisionedMachineList{}
	if err := r.client.List(ctx, machines,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
	); err != nil {
		return nil, fmt.Errorf("failed to list ProvisionedMachines: %w", err)
	}
	return machines.Items, nil
}
