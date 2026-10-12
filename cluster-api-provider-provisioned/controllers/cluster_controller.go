// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/dsx-ai-factory/nvke/cluster-api-provider-provisioned/api/v1alpha1"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedclusters,verbs=get;list;patch;update;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedclusters/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedmachines,verbs=get;list;watch

// ClusterReconciler validates the inventory of a ProvisionedCluster and
// reports the cluster as provisioned.
//
// Reporting the cluster is not optional. The core Cluster API machine
// controller waits for status.initialization.provisioned on the infrastructure
// cluster before it acts on any machine, so a cluster that never reports it
// stops every node of the cluster.
type ClusterReconciler struct {
	client client.Client
	logger *slog.Logger
}

// NewClusterReconciler builds a ClusterReconciler.
func NewClusterReconciler(k8sClient client.Client, logger *slog.Logger) *ClusterReconciler {
	return &ClusterReconciler{
		client: k8sClient,
		logger: logger,
	}
}

// SetupWithManager registers the reconciler with a manager.
//
// The reconciler also watches the machines of the provider, because the
// reported allocations change when a machine takes or releases a host.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ProvisionedCluster{}).
		Watches(
			&infrav1.ProvisionedMachine{},
			handler.EnqueueRequestsFromMapFunc(machineToCluster),
		).
		Named("provisioned-cluster").
		Complete(r)
}

// machineToCluster maps a machine to the cluster that owns its inventory.
func machineToCluster(_ context.Context, obj client.Object) []reconcile.Request {
	clusterName := obj.GetLabels()[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: clusterName},
	}}
}

// Reconcile validates the inventory and reports the state of the cluster.
//
// The provider holds no finalizer. It creates nothing, so a deleted cluster
// needs no cleanup.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.logger.With("namespace", req.Namespace, "provisionedCluster", req.Name)

	infraCluster := &infrav1.ProvisionedCluster{}
	if err := r.client.Get(ctx, req.NamespacedName, infraCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ProvisionedCluster: %w", err)
	}

	if !infraCluster.DeletionTimestamp.IsZero() {
		logger.Debug("ProvisionedCluster is being deleted, skipping")
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(infraCluster.DeepCopy())

	if err := validateInventory(infraCluster.Spec.Hosts); err != nil {
		// Do not clear a provisioned flag that was already reported. The
		// machines of the cluster are running, and withdrawing the flag would
		// stop Cluster API from acting on them.
		logger.Error("Inventory is not valid", "error", err)
		setCondition(infraCluster, metav1.ConditionFalse, infrav1.InventoryInvalidReason, err.Error())
		if statusErr := r.client.Status().Patch(ctx, infraCluster, patch); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update the status of ProvisionedCluster: %w", statusErr)
		}
		// The inventory only changes when an operator edits it, and that edit
		// triggers a new reconcile. Report the problem and wait.
		return ctrl.Result{}, nil
	}

	machines := &infrav1.ProvisionedMachineList{}
	if err := r.client.List(ctx, machines,
		client.InNamespace(infraCluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: infraCluster.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ProvisionedMachines: %w", err)
	}
	allocated := allocationsFromMachines(infraCluster.Name, machines.Items)

	infraCluster.Status.Initialization.Provisioned = ptr.To(true)
	infraCluster.Status.AllocatedHosts = sortedAllocations(infraCluster.Spec.Hosts, allocated)
	infraCluster.Status.AvailableHosts = availableHostCount(infraCluster.Spec.Hosts, allocated)
	setCondition(infraCluster, metav1.ConditionTrue, infrav1.InventoryValidReason,
		fmt.Sprintf("Inventory holds %d hosts, %d free",
			len(infraCluster.Spec.Hosts), infraCluster.Status.AvailableHosts))

	if err := r.client.Status().Patch(ctx, infraCluster, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update the status of ProvisionedCluster: %w", err)
	}

	logger.Debug("Reconciled ProvisionedCluster",
		"hosts", len(infraCluster.Spec.Hosts),
		"available", infraCluster.Status.AvailableHosts,
	)

	return ctrl.Result{}, nil
}
