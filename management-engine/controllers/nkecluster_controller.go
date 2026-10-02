// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

var nkeClusterOwnedConditions = []string{
	clusterv1.AvailableCondition,
	internalv1.SyncedCondition,
	internalv1.ReadyCondition,
	clusterv1.PausedCondition,
	clusterv1.DeletingCondition,
}

// NKEClusterReconciler reconciles a NKECluster object
type NKEClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=internal.nke.nvidia.com,resources=nkeclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=internal.nke.nvidia.com,resources=nkeclusters/status,verbs=get;update;patch

func (r *NKEClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("reconciling NKECluster")

	var cluster internalv1.NKECluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&cluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if conditionErr := setNKEClusterConditions(&cluster); conditionErr != nil {
			retErr = errors.Join(retErr, conditionErr)
		}
		if patchErr := patchHelper.Patch(ctx, &cluster,
			patch.WithOwnedConditions{Conditions: nkeClusterOwnedConditions},
			patch.WithStatusObservedGeneration{},
		); patchErr != nil {
			retErr = errors.Join(retErr, patchErr)
		}
	}()

	if annotations.HasPaused(&cluster) {
		conditions.Set(&cluster, metav1.Condition{
			Type: clusterv1.PausedCondition, Status: metav1.ConditionTrue, Reason: clusterv1.PausedReason,
		})
		return ctrl.Result{}, nil
	}
	conditions.Set(&cluster, metav1.Condition{
		Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason,
	})
	return ctrl.Result{}, nil
}

func setNKEClusterConditions(cluster *internalv1.NKECluster) error {
	deleting := metav1.ConditionFalse
	reason := clusterv1.NotDeletingReason
	if !cluster.DeletionTimestamp.IsZero() {
		deleting = metav1.ConditionTrue
		reason = clusterv1.DeletingReason
	}
	conditions.Set(cluster, metav1.Condition{Type: clusterv1.DeletingCondition, Status: deleting, Reason: reason})

	if err := conditions.SetSummaryCondition(cluster, cluster, internalv1.SyncedCondition,
		conditions.ForConditionTypes{internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				internalv1.NotSyncedReason, internalv1.SyncUnknownReason, internalv1.SyncedReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NKECluster Synced condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(cluster, cluster, clusterv1.AvailableCondition,
		conditions.ForConditionTypes{internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				clusterv1.NotAvailableReason, clusterv1.AvailableUnknownReason, clusterv1.AvailableReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NKECluster Available condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(cluster, cluster, internalv1.ReadyCondition,
		conditions.ForConditionTypes{clusterv1.AvailableCondition, clusterv1.DeletingCondition},
		conditions.NegativePolarityConditionTypes{clusterv1.DeletingCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(clusterv1.DeletingCondition)),
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				clusterv1.NotReadyReason, clusterv1.ReadyUnknownReason, clusterv1.ReadyReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NKECluster Ready condition: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NKEClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&internalv1.NKECluster{}).
		Named("nkecluster").
		Complete(r)
}
