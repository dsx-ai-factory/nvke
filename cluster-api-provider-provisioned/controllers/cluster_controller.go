// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedclusters,verbs=get;list;patch;update;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedclusters/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedmachines,verbs=get;list;watch

// ClusterReconciler reconciles ProvisionedCluster objects.
type ClusterReconciler struct {
	client client.Client
	logger *slog.Logger
}

// NewClusterReconciler builds a ClusterReconciler.
func NewClusterReconciler(k8sClient client.Client, logger *slog.Logger) *ClusterReconciler {
	return &ClusterReconciler{client: k8sClient, logger: logger}
}

// SetupWithManager registers the reconciler with a manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ProvisionedCluster{}).
		Named("provisioned-cluster").
		Complete(r)
}

// Reconcile handles a ProvisionedCluster reconciliation request.
func (r *ClusterReconciler) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}
