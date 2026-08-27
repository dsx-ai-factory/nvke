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

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedmachines,verbs=get;list;patch;update;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedmachines/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=provisionedhostclaims,verbs=create;delete;get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// MachineReconciler reconciles ProvisionedMachine objects.
type MachineReconciler struct {
	client    client.Client
	apiReader client.Reader
	logger    *slog.Logger
}

// NewMachineReconciler builds a MachineReconciler.
func NewMachineReconciler(k8sClient client.Client, apiReader client.Reader, logger *slog.Logger) *MachineReconciler {
	return &MachineReconciler{client: k8sClient, apiReader: apiReader, logger: logger}
}

// SetupWithManager registers the reconciler with a manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ProvisionedMachine{}).
		Named("provisioned-machine").
		Complete(r)
}

// Reconcile handles a ProvisionedMachine reconciliation request.
func (r *MachineReconciler) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}
